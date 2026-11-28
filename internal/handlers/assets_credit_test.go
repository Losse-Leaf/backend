package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
)

func TestCalculateElapsedInstallmentMonthsUsesBeijingDate(t *testing.T) {
	purchase := time.Date(2026, 6, 2, 0, 0, 0, 0, config.BeijingLocation)
	// 北京时间 9/25 00:30（UTC 仍是 9/24）：9/25 这一期已经到期，应计 4 期。
	checkTime := time.Date(2026, 9, 25, 0, 30, 0, 0, config.BeijingLocation).UTC()
	paid, remain := CalculateElapsedInstallmentMonths(purchase, 12, 25, checkTime)
	if paid != 4 || remain != 8 {
		t.Fatalf("UTC 服务器应按北京时间切分还款日: paid=%d remain=%d, want 4/8", paid, remain)
	}
}

func TestAssetsCreditAndDesensitization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	enableAllPublicModulesForTest(t)

	// 注册测试受信任设备凭证 (BE-18)
	testMasterKey := "lldt_test_device_token_123"
	seedTestAdminDevice(testMasterKey)

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		// 公开路由
		v1.GET("/profile", GetCuratorProfile)
		v1.GET("/wishes", GetWishes)
		v1.POST("/wishes", CreateWish)
		v1.POST("/wishes/:id/resolve", ResolveWish)

		// 管理员鉴权受保护路由组 (RC-53)
		write := v1.Group("")
		write.Use(middleware.AdminAuthRequired())
		{
			write.GET("/transactions", GetTransactions)
			write.GET("/transactions/summary", GetTransactionSummary)
			write.POST("/transactions", CreateTransaction)

			write.GET("/credit-channels", GetCreditChannels)
			write.POST("/credit-channels", CreateCreditChannel)
			write.PUT("/credit-channels/:id", UpdateCreditChannel)
			write.DELETE("/credit-channels/:id", DeleteCreditChannel)
		}
	}

	// 1. 信贷渠道管理 (Credit Channels CRUD)
	t.Run("Credit Channels CRUD", func(t *testing.T) {
		// 1.1 未授权访问返回 401
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/credit-channels", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 on unauthorized credit-channels, got %d", w.Code)
		}

		// 1.2 带 Token 查询内置渠道
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/credit-channels", nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on authorized credit-channels, got %d", w.Code)
		}
		var listResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &listResp)
		channels := listResp["data"].([]interface{})
		if len(channels) < 2 {
			t.Fatalf("expected at least 2 builtin channels, got %d", len(channels))
		}

		// 1.3 新建自定义渠道 (招行信用卡，15 号还款)
		createPayload := CreditChannelRequest{
			Code:         "cmb_card",
			Name:         "招行信用卡",
			RepaymentDay: 15,
			Color:        "var(--accent-indigo)",
		}
		body, _ := json.Marshal(createPayload)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, "/api/v1/credit-channels", bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create credit channel, got %d: %s", w.Code, w.Body.String())
		}
		var chResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &chResp)
		createdCh := chResp["data"].(map[string]interface{})
		createdID := uint(createdCh["id"].(float64))

		// 1.4 修改自定义渠道还款日为 18 号
		updatePayload := CreditChannelRequest{
			Name:         "招行经典白",
			RepaymentDay: 18,
		}
		body, _ = json.Marshal(updatePayload)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/credit-channels/%d", createdID), bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on update credit channel, got %d", w.Code)
		}

		// 1.4b 修改渠道 code 并同步迁移关联账目
		db.DB.Create(&models.Transaction{
			Title:                  "招行买书",
			Amount:                 20000,
			Type:                   "expense",
			IsInstallment:          true,
			InstallmentChannelCode: "cmb_card",
			OccurredAt:             time.Now(),
		})
		updateCodePayload := CreditChannelRequest{
			Code:         "cmb_visa",
			Name:         "招行经典白 VISA",
			RepaymentDay: 18,
		}
		body, _ = json.Marshal(updateCodePayload)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/credit-channels/%d", createdID), bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 updating channel code: %d", w.Code)
		}
		var checkTx models.Transaction
		db.DB.Where("title = ?", "招行买书").First(&checkTx)
		if checkTx.InstallmentChannelCode != "cmb_visa" {
			t.Fatalf("expected transaction channel code to be migrated to cmb_visa, got %s", checkTx.InstallmentChannelCode)
		}

		// 1.5 尝试删除内置渠道（应该被拒绝 400）
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodDelete, "/api/v1/credit-channels/1", nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 when deleting builtin channel, got %d", w.Code)
		}

		// 1.6 删除自定义渠道
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/credit-channels/%d", createdID), nil)
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 when deleting custom channel, got %d", w.Code)
		}
	})

	// 2. 访客端绝对隔离 (Visitor Auth Isolation RC-53)
	t.Run("Visitor Auth Isolation", func(t *testing.T) {
		// 访客请求 GET /api/v1/transactions 必须返回 401 Unauthorized (RC-53)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/transactions", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("SECURITY VIOLATION: guest must get 401 on transactions, got %d", w.Code)
		}
	})

	// 3. 记账分期付款与心愿联动支持 (RC-53)
	t.Run("Create Transaction with Installment and Wish Linkage", func(t *testing.T) {
		txReq := TransactionRequest{
			Amount:                 239900,
			Type:                   "expense",
			Category:               "购物",
			Title:                  "AirPods Max 银色",
			Note:                   "头戴式主动降噪耳机",
			PaymentMethod:          "花呗分期",
			IsInstallment:          boolPtr(true),
			InstallmentChannelCode: "huabei",
			InstallmentTotalAmount: 239900,
			InstallmentMonths:      12,
			RepaymentDay:           10,
		}
		body, _ := json.Marshal(txReq)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create transaction with installment, got %d", w.Code)
		}

		// 验证关联心愿单时自动闭环流转为 fulfilled (RG-14)
		wishItem := models.Wish{Title: "购买头戴降噪耳机", Kind: "item", Budget: 250000, Status: "wanting"}
		db.DB.Create(&wishItem)

		txWishReq := TransactionRequest{
			Amount:        239900,
			Type:          "expense",
			Category:      "购物",
			Title:         "AirPods Max 银色",
			PaymentMethod: "花呗",
			WishID:        &wishItem.ID,
		}
		bodyWish, _ := json.Marshal(txWishReq)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewBuffer(bodyWish))
		req.Header.Set("Authorization", "Bearer "+testMasterKey)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create transaction with wish_id, got %d", w.Code)
		}

		var checkWish models.Wish
		db.DB.First(&checkWish, wishItem.ID)
		if checkWish.Status != "fulfilled" || checkWish.TransactionID == nil {
			t.Fatalf("expected wish to be auto-fulfilled via wish_id linkage, got status: %s", checkWish.Status)
		}
	})

	// 4. 心愿单归册测试 (RC-53 Scenarios)
	t.Run("Wish Resolution Scenarios", func(t *testing.T) {
		// 4.1 情景 1: 原配原价达成 (exact)
		wish1 := models.Wish{Title: "Sony 24-70 GM2", Kind: "item", Budget: 1599900, Status: "wanting"}
		db.DB.Create(&wish1)

		resReq1 := ResolveWishRequest{
			ResolutionType:    "exact",
			ActualCost:        1599900,
			CreateTransaction: true,
		}
		body, _ := json.Marshal(resReq1)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wish1.ID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on exact wish resolve, got %d", w.Code)
		}
		var updatedWish1 models.Wish
		db.DB.First(&updatedWish1, wish1.ID)
		if updatedWish1.Status != "fulfilled" || updatedWish1.ResolutionType != "exact" || updatedWish1.ActualCost != 1599900 {
			t.Fatalf("wish 1 exact resolution failed: %+v", updatedWish1)
		}

		// 4.2 情景 2: 改选替代方案 (altered)
		wish2 := models.Wish{Title: "MacBook Pro 16", Kind: "item", Budget: 1999900, Status: "wanting"}
		db.DB.Create(&wish2)

		resReq2 := ResolveWishRequest{
			ResolutionType:    "altered",
			ActualTitle:       "MacBook Pro 14 M3 Pro",
			ActualCost:        1499900,
			CreateTransaction: true,
		}
		body, _ = json.Marshal(resReq2)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wish2.ID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on altered wish resolve, got %d", w.Code)
		}
		var updatedWish2 models.Wish
		db.DB.First(&updatedWish2, wish2.ID)
		if updatedWish2.Status != "fulfilled" || updatedWish2.ResolutionType != "altered" || updatedWish2.ActualTitle != "MacBook Pro 14 M3 Pro" {
			t.Fatalf("wish 2 altered resolution failed: %+v", updatedWish2)
		}

		// 4.3 情景 3: 理性拔草 / 释怀放弃 (relinquished)
		wish3 := models.Wish{Title: "劳力士黑水鬼", Kind: "item", Budget: 8000000, Status: "wanting"}
		db.DB.Create(&wish3)

		resReq3 := ResolveWishRequest{
			ResolutionType:  "relinquished",
			ActualCost:      8000000, // 即使前端传了非 0，后端也必须强制归零
			CreateLifeEntry: true,
			EntryContent:    "审视内心后决定放下物欲，心无旁骛方显从容。",
		}
		body, _ = json.Marshal(resReq3)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wish3.ID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on relinquished wish resolve, got %d", w.Code)
		}
		var updatedWish3 models.Wish
		db.DB.First(&updatedWish3, wish3.ID)
		if updatedWish3.Status != "relinquished" || updatedWish3.ActualCost != 0 || updatedWish3.TransactionID != nil {
			t.Fatalf("relinquished wish must have actual_cost=0 and no tx, got: %+v", updatedWish3)
		}

		// 4.4 情景 4: 心愿达成 + 分期信贷 (RC-53)
		wish4 := models.Wish{Title: "iPad Pro 11 M4", Kind: "item", Spec: "256GB 黑色", Budget: 899900, Status: "wanting"}
		db.DB.Create(&wish4)

		resReq4 := ResolveWishRequest{
			ResolutionType:         "exact",
			ActualCost:             899900,
			CreateTransaction:      true,
			IsInstallment:          true,
			InstallmentChannelCode: "baitiao",
			InstallmentMonths:      12,
			RepaymentDay:           20,
		}
		body, _ = json.Marshal(resReq4)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wish4.ID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on installment wish resolve, got %d", w.Code)
		}
		var updatedWish4 models.Wish
		db.DB.First(&updatedWish4, wish4.ID)
		if updatedWish4.TransactionID == nil {
			t.Fatalf("expected wish4 to have transaction_id linked, got nil")
		}
		var wish4Tx models.Transaction
		db.DB.First(&wish4Tx, *updatedWish4.TransactionID)
		if !wish4Tx.IsInstallment || wish4Tx.InstallmentChannelCode != "baitiao" || wish4Tx.RepaymentDay != 20 {
			t.Fatalf("wish4 transaction installment data incorrect: %+v", wish4Tx)
		}
	})

	// 6. 分期负债按购买时间 (OccurredAt) 与每月还款日核算，且消除整除尾差
	t.Run("Installment Liability Calculated By Purchase Date Without Rounding Drift", func(t *testing.T) {
		loc := time.Local
		purchaseTime := time.Date(2026, 6, 2, 0, 0, 0, 0, loc)
		checkTime := time.Date(2026, 10, 12, 11, 15, 0, 0, loc)

		// 6月2日购买，每月25日还款，截至10月12日已过 6/25, 7/25, 8/25, 9/25 共 4 期，剩余 8 期
		paid, remain := CalculateElapsedInstallmentMonths(purchaseTime, 12, 25, checkTime)
		if paid != 4 || remain != 8 {
			t.Fatalf("expected paid=4, remain=8 for 2026-06-02 to 2026-10-12 (day 25), got paid=%d, remain=%d", paid, remain)
		}

		// 总额 916900 分 (¥9,169.00)，12期每期 76408 分：
		// 0 期已还时，待还必须精确等于 916900 分（杜绝 76408*12=916896 导致少 4 分尾差）
		paid0, debt0 := CalculateInstallmentAmounts(916900, 12, 76408, 0, 12)
		if paid0 != 0 || debt0 != 916900 {
			t.Fatalf("expected paid0=0, debt0=916900, got paid0=%d, debt0=%d", paid0, debt0)
		}

		// 4 期已还时，已还 = 4*76408 = 305632 分，剩余待还 = 916900 - 305632 = 611268 分 (¥6,112.68)
		paid4, debt4 := CalculateInstallmentAmounts(916900, 12, 76408, 4, 8)
		if paid4 != 305632 || debt4 != 611268 || (paid4+debt4) != 916900 {
			t.Fatalf("expected paid4=305632, debt4=611268, got paid4=%d, debt4=%d", paid4, debt4)
		}
	})
}

// boolPtr 便于在结构体字面量里构造 *bool
func boolPtr(v bool) *bool { return &v }
