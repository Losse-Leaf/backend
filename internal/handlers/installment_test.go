package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
)

func setupInstallmentTestDB(t *testing.T) {
	setupTestFullDB(t)
	var profile models.CuratorProfile
	db.DB.FirstOrCreate(&profile)
	var siteCfg models.SiteConfig
	db.DB.FirstOrCreate(&siteCfg)
	var feeCat models.CategoryOption
	db.DB.Where("scope = ? AND name = ?", "expense", "利息手续费").FirstOrCreate(&feeCat, models.CategoryOption{
		Scope:     "expense",
		Name:      "利息手续费",
		IsBuiltin: true,
	})
}

// setupInstallmentRouter 创建测试路由
func setupInstallmentRouter(masterKey string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		write := v1.Group("")
		write.Use(middleware.AdminAuthRequired())
		{
			write.GET("/transactions", GetTransactions)
			write.GET("/transactions/summary", GetTransactionSummary)
			write.POST("/transactions", CreateTransaction)
			write.PUT("/transactions/:id", UpdateTransaction)
			write.DELETE("/transactions/:id", DeleteTransaction)

			write.GET("/assets/overview", GetAssetsOverview)
			write.PUT("/assets/liquid-balance", SetInitialLiquidBalance)
			write.GET("/credit-channels", GetCreditChannels)
			write.DELETE("/categories/:id", DeleteCategory)

			// 分期接口
			write.POST("/installment-plans", CreateInstallmentPlan)
			write.GET("/installment-plans", GetInstallmentPlans)
			write.GET("/installment-plans/summary", GetInstallmentSummary)
			write.GET("/installment-items/upcoming", GetUpcomingInstallmentItems)
			write.GET("/installment-plans/:id", GetInstallmentPlanDetail)
			write.PUT("/installment-plans/:id", UpdateInstallmentPlan)
			write.DELETE("/installment-plans/:id", DeleteInstallmentPlan)
			write.PUT("/installment-items/:id", UpdateInstallmentItem)
			write.POST("/installment-items/:id/pay", PayInstallmentItem)
			write.POST("/installment-items/:id/unpay", UnpayInstallmentItem)
			write.POST("/installment-plans/:id/settle", SettleInstallmentPlan)
		}
	}
	return r
}

// 1. 均摊计算单元测试：整除与余数均摊到最后一期
func TestSplitInstallmentPeriods_Remainder(t *testing.T) {
	// 本金 1000 元 (100000分)，3 期
	pParts, fParts, totalFee, err := SplitInstallmentPeriods(100000, 3, "total_fee", 1000)
	if err != nil {
		t.Fatalf("SplitInstallmentPeriods error: %v", err)
	}

	if len(pParts) != 3 || len(fParts) != 3 {
		t.Fatalf("期数应为 3: len(p)=%d, len(f)=%d", len(pParts), len(fParts))
	}

	// 100000 / 3 = 33333 余 1 -> [33333, 33333, 33334]
	if pParts[0] != 33333 || pParts[1] != 33333 || pParts[2] != 33334 {
		t.Errorf("本金均摊余数未正确加到最后一期: got %v", pParts)
	}
	sumP := pParts[0] + pParts[1] + pParts[2]
	if sumP != 100000 {
		t.Errorf("各期本金之和不等于总本金: sum=%d want=100000", sumP)
	}

	// 1000 / 3 = 333 余 1 -> [333, 333, 334]
	if fParts[0] != 333 || fParts[1] != 333 || fParts[2] != 334 {
		t.Errorf("手续费均摊余数未正确加到最后一期: got %v", fParts)
	}
	sumF := fParts[0] + fParts[1] + fParts[2]
	if sumF != 1000 || totalFee != 1000 {
		t.Errorf("各期手续费之和不等于总手续费: sum=%d, totalFee=%d, want=1000", sumF, totalFee)
	}
}

// 2. 月末还款日顺延与截断测试
func TestCalculateDueDate_MonthEndClamp(t *testing.T) {
	// 首期 2026-01-31，repayDay=31
	d1, err := CalculateDueDate("2026-01-31", 31, 1)
	if err != nil || d1 != "2026-01-31" {
		t.Errorf("第 1 期到期日错误: got %s want 2026-01-31 (err: %v)", d1, err)
	}

	// 第 2 期：2026 年 2 月最大 28 天，截断为 2026-02-28
	d2, err := CalculateDueDate("2026-01-31", 31, 2)
	if err != nil || d2 != "2026-02-28" {
		t.Errorf("第 2 期 2 月应截断到 28 日: got %s want 2026-02-28 (err: %v)", d2, err)
	}

	// 第 3 期：2026 年 3 月有 31 天，取 2026-03-31
	d3, err := CalculateDueDate("2026-01-31", 31, 3)
	if err != nil || d3 != "2026-03-31" {
		t.Errorf("第 3 期 3 月应为 31 日: got %s want 2026-03-31 (err: %v)", d3, err)
	}

	// 第 4 期：2026 年 4 月有 30 天，截断为 2026-04-30
	d4, err := CalculateDueDate("2026-01-31", 31, 4)
	if err != nil || d4 != "2026-04-30" {
		t.Errorf("第 4 期 4 月应截断到 30 日: got %s want 2026-04-30 (err: %v)", d4, err)
	}
}

// 3. 用户指定的数值用例（必须通过的测试用例）：
// 本金 ¥1,200，3 期，每期固定手续费 ¥6，类型 A，渠道花呗（25 日），首期 11 月 25 日：
// - 创建后：固定资产 +1,200，待还负债 1,200，流动资金不变，净资产不变，月支出不变
// - 标记第 1 期已还：流动资金 −406，负债 800，该月支出 +6，净资产 −6
// - 全部还完：流动资金累计 −1,218，负债 0，手续费支出合计 18
// - 重复标记同一期：不得重复入账；撤销已还：金额与账目全部回滚
func TestInstallmentUserNumericalCase(t *testing.T) {
	setupInstallmentTestDB(t)
	token := "token_test_installment_num_123"
	seedTestAdminDevice(token)
	r := setupInstallmentRouter(token)

	// 设置花呗渠道还款日为 25
	db.DB.Model(&models.CreditChannel{}).Where("code = ?", "huabei").Update("repayment_day", 25)

	// 初始流动资金设为 0
	db.DB.Model(&models.SiteConfig{}).Where("id = 1").Update("initial_liquid_balance", 0)

	// Step 1: 创建分期计划 (本金 120000分, 3期, 每期固定600分, 类型 A, 首期 2026-11-25)
	createReq := CreateInstallmentPlanRequest{
		ChannelCode:   "huabei",
		Title:         "MacBook Air M3",
		Kind:          "asset",
		Principal:     120000, // ¥1,200.00
		Periods:       3,
		FeeMode:       "fixed_per_period",
		FeeValue:      600, // ¥6.00
		FirstDueDate:  "2026-11-25",
		RepayDay:      25,
		CreateNewGear: true,
		GearName:      "MacBook Air M3",
		GearCategory:  "数码器材",
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/installment-plans", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("创建分期失败: code=%d body=%s", w.Code, w.Body.String())
	}

	var createResp struct {
		Data struct {
			Plan  models.InstallmentPlan   `json:"plan"`
			Items []models.InstallmentItem `json:"items"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	plan := createResp.Data.Plan
	items := createResp.Data.Items

	if len(items) != 3 {
		t.Fatalf("期次数量应为 3, got %d", len(items))
	}
	for i, it := range items {
		if it.PrincipalPart != 40000 || it.FeePart != 600 {
			t.Errorf("期次 %d 金额错误: p=%d f=%d, want p=40000, f=600", i+1, it.PrincipalPart, it.FeePart)
		}
	}

	// 验证 Checkpoint 1：创建后大盘状态
	// 固定资产 +1,200，待还负债 1,200，流动资金不变 (0)，净资产不变 (0)，月支出不变 (0)
	getOverview := func() AssetsOverviewResponse {
		reqO, _ := http.NewRequest("GET", "/api/v1/assets/overview", nil)
		reqO.Header.Set("Authorization", "Bearer "+token)
		wO := httptest.NewRecorder()
		r.ServeHTTP(wO, reqO)
		if wO.Code != http.StatusOK {
			t.Fatalf("获取大盘失败: code=%d, body=%s", wO.Code, wO.Body.String())
		}
		var resp struct {
			Data AssetsOverviewResponse `json:"data"`
		}
		_ = json.Unmarshal(wO.Body.Bytes(), &resp)
		return resp.Data
	}

	ov1 := getOverview()
	if ov1.FixedAssets != 120000 {
		t.Errorf("Checkpoint 1: 固定资产应为 120000 (+1,200元), got %d", ov1.FixedAssets)
	}
	if ov1.TotalLiabilities != 120000 {
		t.Errorf("Checkpoint 1: 待还负债应为 120000 (1,200元), got %d", ov1.TotalLiabilities)
	}
	if ov1.LiquidBalance != 0 {
		t.Errorf("Checkpoint 1: 流动资金应为 0, got %d", ov1.LiquidBalance)
	}
	if ov1.NetWorth != 0 {
		t.Errorf("Checkpoint 1: 净资产应为 0, got %d", ov1.NetWorth)
	}

	// 验证 Checkpoint 1 月度支出为 0
	getMonthlySummary := func(y, m int) (int64, int64) {
		reqS, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/transactions/summary?year=%04d&month=%02d", y, m), nil)
		reqS.Header.Set("Authorization", "Bearer "+token)
		wS := httptest.NewRecorder()
		r.ServeHTTP(wS, reqS)
		var resp struct {
			Data struct {
				TotalExpense int64 `json:"total_expense"`
				TotalIncome  int64 `json:"total_income"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wS.Body.Bytes(), &resp)
		return resp.Data.TotalExpense, resp.Data.TotalIncome
	}
	expNov, _ := getMonthlySummary(2026, 11)
	if expNov != 0 {
		t.Errorf("Checkpoint 1: 创建分期不产生月支出, got %d, want 0", expNov)
	}

	// Step 2: 标记第 1 期已还 (应还 40600 = 40000 + 600)
	item1 := items[0]
	payReq := PayInstallmentItemRequest{
		PayAccount: "招行储蓄卡",
		PaidAt:     "2026-11-25T10:00:00+08:00",
	}
	payBody, _ := json.Marshal(payReq)
	reqP1, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/installment-items/%d/pay", item1.ID), bytes.NewBuffer(payBody))
	reqP1.Header.Set("Authorization", "Bearer "+token)
	reqP1.Header.Set("Content-Type", "application/json")
	wP1 := httptest.NewRecorder()
	r.ServeHTTP(wP1, reqP1)
	if wP1.Code != http.StatusOK {
		t.Fatalf("标记第 1 期已还失败: code=%d body=%s", wP1.Code, wP1.Body.String())
	}

	// 验证 Checkpoint 2：标记第 1 期已还后
	// 流动资金 −406 (-40600分)，负债 800 (80000分)，该月支出 +6 (600分)，净资产 −6 (-600分)
	ov2 := getOverview()
	if ov2.LiquidBalance != -40600 {
		t.Errorf("Checkpoint 2: 流动资金应为 -40600 (-¥406), got %d", ov2.LiquidBalance)
	}
	if ov2.TotalLiabilities != 80000 {
		t.Errorf("Checkpoint 2: 待还负债应为 80000 (¥800), got %d", ov2.TotalLiabilities)
	}
	if ov2.NetWorth != -600 {
		t.Errorf("Checkpoint 2: 净资产应为 -600 (-¥6), got %d", ov2.NetWorth)
	}
	expNov2, _ := getMonthlySummary(2026, 11)
	if expNov2 != 600 {
		t.Errorf("Checkpoint 2: 11 月支出应仅计入手续费 600 (+¥6), got %d", expNov2)
	}

	// 验证 Checkpoint 3：重复标记同一期已还，必须被拒绝 (400)
	wP1Dup := httptest.NewRecorder()
	r.ServeHTTP(wP1Dup, reqP1)
	if wP1Dup.Code != http.StatusBadRequest {
		t.Errorf("Checkpoint 3: 重复标记同一期应返回 400 拦截, got code=%d", wP1Dup.Code)
	}

	// Step 3: 还清第 2 期与第 3 期
	for _, it := range items[1:] {
		pReq := PayInstallmentItemRequest{
			PayAccount: "招行储蓄卡",
			PaidAt:     "2026-11-25T10:00:00+08:00",
		}
		pB, _ := json.Marshal(pReq)
		reqP, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/installment-items/%d/pay", it.ID), bytes.NewBuffer(pB))
		reqP.Header.Set("Authorization", "Bearer "+token)
		wP := httptest.NewRecorder()
		r.ServeHTTP(wP, reqP)
		if wP.Code != http.StatusOK {
			t.Fatalf("还款期次 %d 失败: code=%d body=%s", it.Seq, wP.Code, wP.Body.String())
		}
	}

	// 验证 Checkpoint 4：全部还完后
	// 流动资金累计 −1,218 (-121800分)，负债 0，手续费支出合计 18 (1800分)，净资产累计 −18 (-1800分)
	ov3 := getOverview()
	if ov3.LiquidBalance != -121800 {
		t.Errorf("Checkpoint 4: 流动资金累计应为 -121800 (-¥1,218), got %d", ov3.LiquidBalance)
	}
	if ov3.TotalLiabilities != 0 {
		t.Errorf("Checkpoint 4: 待还负债应为 0, got %d", ov3.TotalLiabilities)
	}
	if ov3.NetWorth != -1800 {
		t.Errorf("Checkpoint 4: 净资产应为 -1800 (-¥18), got %d", ov3.NetWorth)
	}
	expNovAll, _ := getMonthlySummary(2026, 11)
	if expNovAll != 1800 {
		t.Errorf("Checkpoint 4: 手续费支出合计应为 1800 (¥18), got %d", expNovAll)
	}

	// Step 4: 撤销第 1 期已还测试
	// 验证金额与账目全部回滚
	reqUnpay, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/installment-items/%d/unpay", item1.ID), nil)
	reqUnpay.Header.Set("Authorization", "Bearer "+token)
	wUnpay := httptest.NewRecorder()
	r.ServeHTTP(wUnpay, reqUnpay)
	if wUnpay.Code != http.StatusOK {
		t.Fatalf("撤销第 1 期还款失败: code=%d body=%s", wUnpay.Code, wUnpay.Body.String())
	}

	ov4 := getOverview()
	// 流动资金应回退 +406 -> 从 -121800 变为 -81200
	if ov4.LiquidBalance != -81200 {
		t.Errorf("Checkpoint 5: 撤销第 1 期后流动资金应回退到 -81200 (-¥812), got %d", ov4.LiquidBalance)
	}
	// 负债应恢复 +400 -> 40000分 (¥400)
	if ov4.TotalLiabilities != 40000 {
		t.Errorf("Checkpoint 5: 撤销第 1 期后负债应恢复为 40000 (¥400), got %d", ov4.TotalLiabilities)
	}
	// 净资产应回滚 -> -1200 (-¥12)
	if ov4.NetWorth != -1200 {
		t.Errorf("Checkpoint 5: 撤销第 1 期后净资产应为 -1200 (-¥12), got %d", ov4.NetWorth)
	}
	// 计划状态应自动回退为 "active"
	var planCheck models.InstallmentPlan
	db.DB.First(&planCheck, "id = ?", plan.ID)
	if planCheck.Status != "active" {
		t.Errorf("Checkpoint 5: 撤销还款后计划状态应回退为 active, got %s", planCheck.Status)
	}
}

// 4. 提前结清单元测试
func TestInstallmentAdvanceSettlement(t *testing.T) {
	setupInstallmentTestDB(t)
	token := "token_test_installment_settle_123"
	seedTestAdminDevice(token)
	r := setupInstallmentRouter(token)

	// 创建 3 期计划，本金 1200 元 (每期本金 400 元)
	createReq := CreateInstallmentPlanRequest{
		ChannelCode:  "huabei",
		Title:        "相机镜头",
		Kind:         "asset",
		Principal:    120000,
		Periods:      3,
		FeeMode:      "none",
		FirstDueDate: "2026-11-25",
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/installment-plans", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("创建分期失败: %s", w.Body.String())
	}

	var plan models.InstallmentPlan
	db.DB.First(&plan)

	// 还第 1 期 (40000)
	var item1 models.InstallmentItem
	db.DB.Where("plan_id = ? AND seq = 1", plan.ID).First(&item1)
	reqP, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/installment-items/%d/pay", item1.ID), nil)
	reqP.Header.Set("Authorization", "Bearer "+token)
	wP := httptest.NewRecorder()
	r.ServeHTTP(wP, reqP)
	if wP.Code != http.StatusOK {
		t.Fatalf("还第 1 期失败: %s", wP.Body.String())
	}

	// 剩余本金为 80000
	// 测试提前结清金额低于剩余本金 (70000 < 80000) -> 必须被拒绝 (400)
	settleLowReq := SettleInstallmentPlanRequest{
		PaidAmount: 70000,
		PayAccount: "微信支付",
	}
	sLowBody, _ := json.Marshal(settleLowReq)
	reqSLow, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/installment-plans/%d/settle", plan.ID), bytes.NewBuffer(sLowBody))
	reqSLow.Header.Set("Authorization", "Bearer "+token)
	wSLow := httptest.NewRecorder()
	r.ServeHTTP(wSLow, reqSLow)
	if wSLow.Code != http.StatusBadRequest {
		t.Errorf("结清金额低于剩余本金应被拒绝 400, got code=%d", wSLow.Code)
	}

	// 正常提前结清：实付 82000 (剩余本金 80000 + 手续费差额 2000)
	settleReq := SettleInstallmentPlanRequest{
		PaidAmount: 82000,
		PayAccount: "微信支付",
		PaidAt:     "2026-11-25T12:00:00+08:00",
	}
	sBody, _ := json.Marshal(settleReq)
	reqS, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/installment-plans/%d/settle", plan.ID), bytes.NewBuffer(sBody))
	reqS.Header.Set("Authorization", "Bearer "+token)
	wS := httptest.NewRecorder()
	r.ServeHTTP(wS, reqS)
	if wS.Code != http.StatusOK {
		t.Fatalf("提前结清失败: code=%d body=%s", wS.Code, wS.Body.String())
	}

	// 校验所有期次已置为 paid，计划状态为 settled
	var remainingCount int64
	db.DB.Model(&models.InstallmentItem{}).Where("plan_id = ? AND status = 'scheduled'", plan.ID).Count(&remainingCount)
	if remainingCount != 0 {
		t.Errorf("提前结清后不应有 scheduled 期次, 仍有 %d 期", remainingCount)
	}

	db.DB.First(&plan, "id = ?", plan.ID)
	if plan.Status != "settled" {
		t.Errorf("提前结清后计划状态应为 settled, got %s", plan.Status)
	}

	// 校验结清账目与手续费
	var settleTx models.Transaction
	db.DB.Where("installment_plan_id = ? AND title LIKE '%提前结清%'", plan.ID).First(&settleTx)
	if settleTx.Amount != 82000 || settleTx.PrincipalAmount != 80000 || settleTx.FeeAmount != 2000 {
		t.Errorf("结清流水记录错误: Amount=%d Principal=%d Fee=%d", settleTx.Amount, settleTx.PrincipalAmount, settleTx.FeeAmount)
	}
}

// 5. 实付金额与保护约束测试
func TestInstallmentProtectionConstraints(t *testing.T) {
	setupInstallmentTestDB(t)
	token := "token_test_installment_guard_123"
	seedTestAdminDevice(token)
	r := setupInstallmentRouter(token)

	// 创建 3 期计划
	createReq := CreateInstallmentPlanRequest{
		ChannelCode:  "huabei",
		Title:        "耳机",
		Kind:         "asset",
		Principal:    60000,
		Periods:      3,
		FeeMode:      "fixed_per_period",
		FeeValue:     500,
		FirstDueDate: "2026-11-25",
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/installment-plans", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("创建分期失败: %s", w.Body.String())
	}

	var plan models.InstallmentPlan
	db.DB.First(&plan)

	// 约束 1：未还款时允许取消计划
	// 先测试有还款后禁止删除
	var item1 models.InstallmentItem
	db.DB.Where("plan_id = ? AND seq = 1", plan.ID).First(&item1)
	reqP, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/installment-items/%d/pay", item1.ID), nil)
	reqP.Header.Set("Authorization", "Bearer "+token)
	wP := httptest.NewRecorder()
	r.ServeHTTP(wP, reqP)
	if wP.Code != http.StatusOK {
		t.Fatalf("还款失败: %s", wP.Body.String())
	}

	// 有还款记录的计划禁止删除 (400)
	reqDelPlan, _ := http.NewRequest("DELETE", fmt.Sprintf("/api/v1/installment-plans/%d", plan.ID), nil)
	reqDelPlan.Header.Set("Authorization", "Bearer "+token)
	wDelPlan := httptest.NewRecorder()
	r.ServeHTTP(wDelPlan, reqDelPlan)
	if wDelPlan.Code != http.StatusBadRequest {
		t.Errorf("已有还款记录的计划不可删除，应返回 400, got code=%d", wDelPlan.Code)
	}

	// 约束 2：分期还款账目不可直接编辑金额
	var repayTx models.Transaction
	db.DB.Where("installment_item_id = ?", item1.ID).First(&repayTx)
	updateTxReq := map[string]interface{}{
		"amount":   99999, // 尝试直接改金额
		"category": "利息手续费",
	}
	uBody, _ := json.Marshal(updateTxReq)
	reqUTx, _ := http.NewRequest("PUT", fmt.Sprintf("/api/v1/transactions/%d", repayTx.ID), bytes.NewBuffer(uBody))
	reqUTx.Header.Set("Authorization", "Bearer "+token)
	wUTx := httptest.NewRecorder()
	r.ServeHTTP(wUTx, reqUTx)
	if wUTx.Code != http.StatusBadRequest {
		t.Errorf("直接编辑还款流水金额应被拦截 400, got code=%d", wUTx.Code)
	}

	// 约束 3：删除还款账目等同于撤销还款
	reqDelTx, _ := http.NewRequest("DELETE", fmt.Sprintf("/api/v1/transactions/%d", repayTx.ID), nil)
	reqDelTx.Header.Set("Authorization", "Bearer "+token)
	wDelTx := httptest.NewRecorder()
	r.ServeHTTP(wDelTx, reqDelTx)
	if wDelTx.Code != http.StatusOK {
		t.Fatalf("删除还款账目失败: %s", wDelTx.Body.String())
	}

	// 校验 item1 是否已联动恢复为 scheduled
	db.DB.First(&item1, "id = ?", item1.ID)
	if item1.Status != "scheduled" {
		t.Errorf("删除还款账目后期次应恢复为 scheduled, got %s", item1.Status)
	}

	// 约束 4：系统内置分类「利息手续费」不可被删除
	var cat models.CategoryOption
	db.DB.Where("name = ?", "利息手续费").First(&cat)
	if cat.ID > 0 {
		reqDelCat, _ := http.NewRequest("DELETE", fmt.Sprintf("/api/v1/categories/%d", cat.ID), nil)
		reqDelCat.Header.Set("Authorization", "Bearer "+token)
		wDelCat := httptest.NewRecorder()
		r.ServeHTTP(wDelCat, reqDelCat)
		if wDelCat.Code != http.StatusBadRequest {
			t.Errorf("删除内置分类「利息手续费」应返回 400, got code=%d", wDelCat.Code)
		}
	}
}

// 6. 类型 B（一笔消费）分期资金流转测试
func TestInstallmentTypeBExpense(t *testing.T) {
	setupInstallmentTestDB(t)
	token := "token_test_installment_type_b_123"
	seedTestAdminDevice(token)
	r := setupInstallmentRouter(token)

	// 初始流动资金设为 100000 (¥1,000.00)
	db.DB.Model(&models.SiteConfig{}).Where("id = 1").Update("initial_liquid_balance", 100000)

	// 创建类型 B「一笔消费」分期 (本金 60000分 ¥600.00，2期，每期固定手续费 1000分 ¥10.00)
	createReq := CreateInstallmentPlanRequest{
		ChannelCode:  "huabei",
		Title:        "年度旅行酒店预订",
		Kind:         "expense",
		CategoryName: "旅行度假",
		Principal:    60000,
		Periods:      2,
		FeeMode:      "fixed_per_period",
		FeeValue:     1000,
		PurchaseDate: "2026-11-01",
		FirstDueDate: "2026-11-25",
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/installment-plans", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("创建类型 B 分期失败: %s", w.Body.String())
	}

	// 验证 Checkpoint 1：购买日产生一笔支出流水，计入 11 月支出 (¥600)，但不扣流动资金 (仍为 ¥1,000)
	reqO, _ := http.NewRequest("GET", "/api/v1/assets/overview", nil)
	reqO.Header.Set("Authorization", "Bearer "+token)
	wO := httptest.NewRecorder()
	r.ServeHTTP(wO, reqO)
	var respO struct {
		Data AssetsOverviewResponse `json:"data"`
	}
	_ = json.Unmarshal(wO.Body.Bytes(), &respO)

	if respO.Data.LiquidBalance != 100000 {
		t.Errorf("类型 B 创建后不应扣流动资金: got %d, want 100000", respO.Data.LiquidBalance)
	}
	if respO.Data.TotalLiabilities != 60000 {
		t.Errorf("类型 B 创建后负债应为 60000 (¥600), got %d", respO.Data.TotalLiabilities)
	}

	reqS, _ := http.NewRequest("GET", "/api/v1/transactions/summary?year=2026&month=11", nil)
	reqS.Header.Set("Authorization", "Bearer "+token)
	wS := httptest.NewRecorder()
	r.ServeHTTP(wS, reqS)
	var respS struct {
		Data struct {
			TotalExpense int64 `json:"total_expense"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wS.Body.Bytes(), &respS)
	if respS.Data.TotalExpense != 60000 {
		t.Errorf("类型 B 购买流水应计入 11 月支出: got %d, want 60000", respS.Data.TotalExpense)
	}

	// 验证 Checkpoint 2：标记第 1 期已还 (30000 本金 + 1000 手续费 = 31000)
	var plan models.InstallmentPlan
	db.DB.Where("kind = ?", "expense").First(&plan)

	var item1 models.InstallmentItem
	db.DB.Where("plan_id = ? AND seq = 1", plan.ID).First(&item1)
	payReq := PayInstallmentItemRequest{
		PayAccount: "招行储蓄卡",
		PaidAt:     "2026-11-25T10:00:00+08:00",
	}
	pBody, _ := json.Marshal(payReq)
	reqP, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/installment-items/%d/pay", item1.ID), bytes.NewBuffer(pBody))
	reqP.Header.Set("Authorization", "Bearer "+token)
	wP := httptest.NewRecorder()
	r.ServeHTTP(wP, reqP)
	if wP.Code != http.StatusOK {
		t.Fatalf("还款失败: %s", wP.Body.String())
	}

	// 流动资金此时扣减 31000 -> 100000 - 31000 = 69000
	wO2 := httptest.NewRecorder()
	r.ServeHTTP(wO2, reqO)
	_ = json.Unmarshal(wO2.Body.Bytes(), &respO)
	if respO.Data.LiquidBalance != 69000 {
		t.Errorf("还款后流动资金应扣减 31000 (¥310): got %d, want 69000", respO.Data.LiquidBalance)
	}

	// 11 月支出：原购买 60000 + 手续费 1000 = 61000 (还款本金 30000 不重复计入月支出)
	wS2 := httptest.NewRecorder()
	r.ServeHTTP(wS2, reqS)
	_ = json.Unmarshal(wS2.Body.Bytes(), &respS)
	if respS.Data.TotalExpense != 61000 {
		t.Errorf("11 月总支出应为 61000 (消费 600 + 手续费 10), got %d", respS.Data.TotalExpense)
	}
}

// 7. 测试编辑分期计划与单期到期日
func TestUpdateInstallmentItemDueDate(t *testing.T) {
	setupInstallmentTestDB(t)
	token := "token_test_installment_edit_123"
	seedTestAdminDevice(token)
	r := setupInstallmentRouter(token)

	// 创建 3 期计划
	createReq := CreateInstallmentPlanRequest{
		ChannelCode:  "huabei",
		Title:        "旧标题",
		Kind:         "asset",
		Principal:    120000,
		Periods:      3,
		FeeMode:      "none",
		FirstDueDate: "2026-11-25",
	}
	body, _ := json.Marshal(createReq)
	req, _ := http.NewRequest("POST", "/api/v1/installment-plans", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("创建分期失败: %s", w.Body.String())
	}

	var plan models.InstallmentPlan
	db.DB.First(&plan)

	// 修改计划标题与备注
	updatePlanReq := UpdateInstallmentPlanRequest{
		Title: "新计划名称",
		Note:  "修改后的备注",
	}
	upBody, _ := json.Marshal(updatePlanReq)
	reqUp, _ := http.NewRequest("PUT", fmt.Sprintf("/api/v1/installment-plans/%d", plan.ID), bytes.NewBuffer(upBody))
	reqUp.Header.Set("Authorization", "Bearer "+token)
	wUp := httptest.NewRecorder()
	r.ServeHTTP(wUp, reqUp)
	if wUp.Code != http.StatusOK {
		t.Fatalf("修改计划标题备注失败: %s", wUp.Body.String())
	}

	var updatedPlan models.InstallmentPlan
	db.DB.First(&updatedPlan, "id = ?", plan.ID)
	if updatedPlan.Title != "新计划名称" || updatedPlan.Note != "修改后的备注" {
		t.Errorf("计划信息未更新: title=%s, note=%s", updatedPlan.Title, updatedPlan.Note)
	}

	// 修改未还期次到期日
	var item1 models.InstallmentItem
	db.DB.Where("plan_id = ? AND seq = 1", plan.ID).First(&item1)

	updateItemReq := UpdateInstallmentItemRequest{
		DueDate: "2026-11-28",
	}
	uiBody, _ := json.Marshal(updateItemReq)
	reqItem, _ := http.NewRequest("PUT", fmt.Sprintf("/api/v1/installment-items/%d", item1.ID), bytes.NewBuffer(uiBody))
	reqItem.Header.Set("Authorization", "Bearer "+token)
	wItem := httptest.NewRecorder()
	r.ServeHTTP(wItem, reqItem)
	if wItem.Code != http.StatusOK {
		t.Fatalf("修改到期日失败: %s", wItem.Body.String())
	}

	var updatedItem models.InstallmentItem
	db.DB.First(&updatedItem, "id = ?", item1.ID)
	if updatedItem.DueDate != "2026-11-28" {
		t.Errorf("到期日未更新: got %s, want 2026-11-28", updatedItem.DueDate)
	}
}

// TestCreateTransactionWithInstallmentGeneratesRealPlan 验证普通创建记账流水带有分期参数时自动生成真实分期计划与期次
func TestCreateTransactionWithInstallmentGeneratesRealPlan(t *testing.T) {
	setupInstallmentTestDB(t)
	token := "token_test_tx_installment_gen_123"
	seedTestAdminDevice(token)
	r := setupInstallmentRouter(token)

	var cat models.CategoryOption
	db.DB.Where("scope = ? AND name = ?", "expense", "数码器材").FirstOrCreate(&cat, models.CategoryOption{
		Scope: "expense",
		Name:  "数码器材",
	})

	// 调用普通创建账目接口，但携带分期参数
	isInst := true
	txReq := map[string]any{
		"amount":                   240000,
		"type":                     "expense",
		"category":                 "数码器材",
		"title":                    "iPhone 16 Pro 分期",
		"is_installment":           isInst,
		"installment_channel_code": "huabei",
		"installment_months":       12,
		"repayment_day":            10,
	}
	body, _ := json.Marshal(txReq)
	req, _ := http.NewRequest("POST", "/api/v1/transactions", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("创建分期账目失败: %s", w.Body.String())
	}

	var res struct {
		Data models.Transaction `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Data.InstallmentPlanID == nil || *res.Data.InstallmentPlanID == 0 {
		t.Fatalf("创建的账目未绑定 installment_plan_id: %+v", res.Data)
	}

	// 验证分期计划与期次记录是否生成
	var plan models.InstallmentPlan
	if err := db.DB.First(&plan, *res.Data.InstallmentPlanID).Error; err != nil {
		t.Fatalf("找不到生成的分期计划: %v", err)
	}
	if plan.Principal != 240000 || plan.Periods != 12 || plan.Kind != "expense" {
		t.Errorf("分期计划字段不符合预期: principal=%d, periods=%d, kind=%s", plan.Principal, plan.Periods, plan.Kind)
	}

	var items []models.InstallmentItem
	db.DB.Where("plan_id = ?", plan.ID).Order("seq ASC").Find(&items)
	if len(items) != 12 {
		t.Fatalf("生成的期次数量不正确: got %d, want 12", len(items))
	}
	for i, itm := range items {
		if itm.PrincipalPart != 20000 {
			t.Errorf("期次 %d 本金均摊错误: got %d, want 20000", i+1, itm.PrincipalPart)
		}
	}
}
