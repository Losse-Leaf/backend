package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// 「编辑账目不能抹掉分期」——这条契约曾经被静默破坏：
// 客户端的 EditTransactionSheet 构造请求时**不传**分期字段，
// 而 `is_installment` 是普通 bool（JSONEncoder 一定会写进请求体），
// 后端据此把 `tx.IsInstallment = false` ——
// 于是**任何一次编辑（哪怕只改标题）都会把分期账目降级成一次性支出**：
// 分期负债凭空消失、流动余额被整额冲减。
func TestUpdateTransactionKeepsInstallmentWhenNotCarried(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)
	if err := db.DB.AutoMigrate(&models.Transaction{}, &models.Device{}); err != nil {
		t.Fatalf("迁移表失败: %v", err)
	}

	token := "lldt_test_installment_keep"
	device := models.Device{
		DeviceID:  "dev_installment_keep",
		Name:      "测试设备",
		Platform:  "macos",
		TokenHash: middleware.HashToken(token),
		Status:    "active",
	}
	if err := db.DB.Create(&device).Error; err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	// 一条 12 期分期账目
	tx := models.Transaction{
		Title:                   "分期买笔记本",
		Amount:                  1200000,
		Type:                    "expense",
		Category:                "数码器材",
		PaymentMethod:           "花呗",
		OccurredAt:              time.Now(),
		IsInstallment:           true,
		InstallmentChannelCode:  "huabei",
		InstallmentTotalAmount:  1200000,
		InstallmentMonths:       12,
		InstallmentMonthlyPay:   100000,
		InstallmentRemainMonths: 12,
		RepaymentDay:            10,
	}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatalf("创建分期账目失败: %v", err)
	}

	r := gin.New()
	r.PUT("/api/v1/transactions/:id", middleware.AdminAuthRequired(), UpdateTransaction)

	// 场景一：请求**完全不带** is_installment（纯 PATCH 语义）
	body, _ := json.Marshal(map[string]any{
		"title":    "只改了标题",
		"category": "数码器材",
		"amount":   1200000,
		"type":     "expense",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/transactions/"+itoa(int(tx.ID)), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}

	var reloaded models.Transaction
	if err := db.DB.First(&reloaded, tx.ID).Error; err != nil {
		t.Fatalf("重新读取失败: %v", err)
	}
	if !reloaded.IsInstallment {
		t.Error("未携带 is_installment 时必须保留分期标记 —— 否则编辑一次就把分期降级了")
	}
	if reloaded.InstallmentMonthlyPay != 100000 {
		t.Errorf("月供不应被清零，实际 %d", reloaded.InstallmentMonthlyPay)
	}
	if reloaded.InstallmentChannelCode != "huabei" {
		t.Errorf("渠道不应被清空，实际 %q", reloaded.InstallmentChannelCode)
	}
	if reloaded.Title != "只改了标题" {
		t.Errorf("标题应被更新，实际 %q", reloaded.Title)
	}

	// 场景二：**老客户端形态** —— 只发 is_installment:false（不携带 clear_installment）
	//
	// 关键：已发布的 mac 客户端（v1.8.1 ~ v2.0.1）的 `isInstallment` 是非可选 Bool，
	// JSONEncoder **必定**写出 `"is_installment": false`，它压根不知道可以不传。
	// 若后端把 false 当成「取消分期」，老用户**编辑一次账目**就会毁掉分期负债：
	// 期数、月供、剩余期数、渠道全部清零，待还总额与流动余额随之失真。
	// 所以单传 false **必须**保持原值。
	body2, _ := json.Marshal(map[string]any{
		"title":          "老客户端只发了 false",
		"category":       "数码器材",
		"amount":         1200000,
		"type":           "expense",
		"is_installment": false,
	})
	req2 := httptest.NewRequest(http.MethodPut, "/api/v1/transactions/"+itoa(int(tx.ID)), bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Device-Token", token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w2.Code, w2.Body.String())
	}
	var stillInstallment models.Transaction
	if err := db.DB.First(&stillInstallment, tx.ID).Error; err != nil {
		t.Fatalf("重新读取失败: %v", err)
	}
	if !stillInstallment.IsInstallment {
		t.Error("只发 is_installment=false（老客户端形态）**不得**降级 —— 否则老用户编辑一次就毁掉分期负债")
	}
	if stillInstallment.InstallmentMonthlyPay != 100000 || stillInstallment.InstallmentMonths != 12 {
		t.Errorf("分期字段应保持不变，实际 monthlyPay=%d months=%d",
			stillInstallment.InstallmentMonthlyPay, stillInstallment.InstallmentMonths)
	}

	// 场景三：显式声明 clear_installment 才真的降级
	body3, _ := json.Marshal(map[string]any{
		"title":             "明确改成一次性",
		"category":          "数码器材",
		"amount":            1200000,
		"type":              "expense",
		"is_installment":    false,
		"clear_installment": true,
	})
	req3 := httptest.NewRequest(http.MethodPut, "/api/v1/transactions/"+itoa(int(tx.ID)), bytes.NewReader(body3))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Device-Token", token)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w3.Code, w3.Body.String())
	}
	var downgraded models.Transaction
	if err := db.DB.First(&downgraded, tx.ID).Error; err != nil {
		t.Fatalf("重新读取失败: %v", err)
	}
	if downgraded.IsInstallment {
		t.Error("显式声明 clear_installment 时应当降级为一次性支出")
	}
	if downgraded.InstallmentMonthlyPay != 0 || downgraded.InstallmentMonths != 0 {
		t.Errorf("降级后分期字段应清零，实际 monthlyPay=%d months=%d",
			downgraded.InstallmentMonthlyPay, downgraded.InstallmentMonths)
	}
}

// 金额必须为正：负数会反向冲减所有汇总
func TestTransactionRejectsNonPositiveAmount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)
	if err := db.DB.AutoMigrate(&models.Transaction{}, &models.Device{}); err != nil {
		t.Fatalf("迁移表失败: %v", err)
	}

	token := "lldt_test_amount_positive"
	device := models.Device{
		DeviceID:  "dev_amount_positive",
		Name:      "测试设备",
		Platform:  "macos",
		TokenHash: middleware.HashToken(token),
		Status:    "active",
	}
	if err := db.DB.Create(&device).Error; err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	r := gin.New()
	r.POST("/api/v1/transactions", middleware.AdminAuthRequired(), CreateTransaction)

	for _, amount := range []int64{-5000, 0} {
		body, _ := json.Marshal(map[string]any{
			"amount":   amount,
			"type":     "expense",
			"category": "餐饮",
			"title":    "负数测试",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Device-Token", token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("金额 %d 应当被拒绝（会反向冲减所有汇总）", amount)
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
