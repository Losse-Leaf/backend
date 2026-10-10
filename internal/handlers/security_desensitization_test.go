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

// TestBE13_AuthenticationBypassPrevention 测试致命级安全鉴权穿透修复 (BE-13)
// 当系统已初始化但 ADMIN_API_KEY 为空时，绝不能降级为 dev_open，必须严格返回 401
func TestBE13_AuthenticationBypassPrevention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	// 1. 初始化站点配置
	var cfg models.SiteConfig
	db.DB.FirstOrCreate(&cfg)
	cfg.Initialized = true
	cfg.MasterPasscode = "$2a$10$abcdefghijklmnopqrstuvwxyz123456" // 已设主理人口令
	db.DB.Save(&cfg)

	// 模拟环境变量 ADMIN_API_KEY 未设置 ("")
	config.AppConfig.AdminAPIKey = ""

	r := gin.New()
	v1 := r.Group("/api/v1")
	protected := v1.Group("")
	protected.Use(middleware.AdminAuthRequired())
	{
		protected.GET("/assets/overview", GetAssetsOverview)
		protected.PUT("/assets/liquid-balance", SetInitialLiquidBalance)
		protected.GET("/transactions", GetTransactions)
	}

	// 访客未带 Token 访问受保护接口必须被拦截 (401)
	endpoints := []string{"/api/v1/assets/overview", "/api/v1/transactions"}
	for _, ep := range endpoints {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, ep, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("BE-13 漏洞暴露: ep %s expected 401 Unauthorized, got %d: %s", ep, w.Code, w.Body.String())
		}
	}

	// 访客未带 Token 访问 PUT 接口必须被拦截 (401)
	wPut := httptest.NewRecorder()
	reqPut, _ := http.NewRequest(http.MethodPut, "/api/v1/assets/liquid-balance", bytes.NewBufferString(`{"initial_liquid_balance": 100}`))
	reqPut.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wPut, reqPut)
	if wPut.Code != http.StatusUnauthorized {
		t.Fatalf("BE-13 漏洞暴露: PUT /assets/liquid-balance expected 401, got %d", wPut.Code)
	}
}

// TestBE14_PublicVisitorFinancialDesensitization 测试公开接口财务隐私脱敏 (BE-14)
// 心愿单、城市回忆抽屉、年度生活年鉴公开只读接口在访客模式下必须严格脱敏
func TestBE14_PublicVisitorFinancialDesensitization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	testMasterKey := "test-admin-secret-999"
	config.AppConfig.AdminAPIKey = testMasterKey
	defer func() {
		config.AppConfig.AdminAPIKey = ""
	}()

	// 标记系统已初始化
	var cfg models.SiteConfig
	db.DB.FirstOrCreate(&cfg)
	cfg.Initialized = true
	db.DB.Save(&cfg)

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		v1.GET("/wishes", GetWishes)
		v1.GET("/wishes/:id", GetWishByID)
		v1.GET("/footprints/cities/:city_code/drawer", GetCityMemoryDrawer)
		v1.GET("/almanac/summary", GetAnnualAlmanacSummary)
	}

	// 准备测试数据:
	// 1. 记账流水
	txID := uint(101)
	txExpense := models.Transaction{
		ID:         txID,
		Amount:     12800, // 128 元
		Type:       "expense",
		Category:   "餐饮美食",
		Title:      "上海弄堂本帮菜",
		CityCode:   "SHA",
		OccurredAt: time.Date(2026, 5, 20, 12, 0, 0, 0, time.Local),
	}
	db.DB.Create(&txExpense)

	txIncome := models.Transaction{
		ID:         102,
		Amount:     80000, // 800 元
		Type:       "income",
		Category:   "工资收入",
		Title:      "兼职稿酬",
		OccurredAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.Local),
	}
	db.DB.Create(&txIncome)

	// 2. 心愿单已达成数据
	gearIDStr := "gear_camera_001"
	fulfilledWish := models.Wish{
		Title:          "富士 X100VI 相机",
		Kind:           "item",
		Budget:         1199900,
		ActualCost:     1250000,
		Status:         "fulfilled",
		ResolutionType: "exact",
		TransactionID:  &txID,
		GearID:         &gearIDStr,
		FulfilledAt:    &txExpense.OccurredAt,
	}
	db.DB.Create(&fulfilledWish)

	// 3. 足迹数据
	footprint := models.Footprint{
		CityCode:  "SHA",
		CityName:  "上海",
		VisitedAt: time.Date(2026, 5, 20, 9, 0, 0, 0, time.Local),
		Note:      "初夏漫步梧桐区",
	}
	db.DB.Create(&footprint)

	// -------------------------------------------------------------
	// A. 检验心愿单脱敏: GET /api/v1/wishes 和 /api/v1/wishes/:id
	// -------------------------------------------------------------
	t.Run("Wishes Visitor Desensitization", func(t *testing.T) {
		// 访客视角列表
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/wishes", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var listResp struct {
			Data struct {
				Wishes []models.Wish `json:"wishes"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &listResp)
		if len(listResp.Data.Wishes) == 0 {
			t.Fatalf("expected at least 1 wish")
		}
		item := listResp.Data.Wishes[0]
		if item.ActualCost != 0 || item.TransactionID != nil || item.GearID != nil {
			t.Fatalf("BE-14 访客列表泄露财务/关联隐私: actual_cost=%d, txID=%v, gearID=%v",
				item.ActualCost, item.TransactionID, item.GearID)
		}

		// 访客视角详情
		wDetail := httptest.NewRecorder()
		reqDetail, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/wishes/%d", fulfilledWish.ID), nil)
		r.ServeHTTP(wDetail, reqDetail)
		if wDetail.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", wDetail.Code)
		}
		var detailResp struct {
			Data models.Wish `json:"data"`
		}
		_ = json.Unmarshal(wDetail.Body.Bytes(), &detailResp)
		if detailResp.Data.ActualCost != 0 || detailResp.Data.TransactionID != nil || detailResp.Data.GearID != nil {
			t.Fatalf("BE-14 访客详情泄露财务隐私: actual_cost=%d", detailResp.Data.ActualCost)
		}

		// 管理员视角必须保留完整数据
		wAdmin := httptest.NewRecorder()
		reqAdmin, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/wishes/%d", fulfilledWish.ID), nil)
		reqAdmin.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(wAdmin, reqAdmin)
		if wAdmin.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", wAdmin.Code)
		}
		var adminResp struct {
			Data models.Wish `json:"data"`
		}
		_ = json.Unmarshal(wAdmin.Body.Bytes(), &adminResp)
		if adminResp.Data.ActualCost != 1250000 || adminResp.Data.TransactionID == nil || adminResp.Data.GearID == nil {
			t.Fatalf("BE-14 管理员未获取到完整财务数据: actual_cost=%d", adminResp.Data.ActualCost)
		}
	})

	// -------------------------------------------------------------
	// B. 检验城市回忆抽屉脱敏: GET /api/v1/footprints/cities/:city_code/drawer
	// -------------------------------------------------------------
	t.Run("Footprints Drawer Visitor Desensitization", func(t *testing.T) {
		// 访客视角
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/footprints/cities/SHA/drawer", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var drawerResp struct {
			Data struct {
				LedgerSummary struct {
					TotalSpent       int64         `json:"total_spent"`
					TransactionCount int           `json:"transaction_count"`
					Categories       []interface{} `json:"categories"`
				} `json:"ledger_summary"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &drawerResp)
		if drawerResp.Data.LedgerSummary.TotalSpent != 0 ||
			drawerResp.Data.LedgerSummary.TransactionCount != 0 ||
			len(drawerResp.Data.LedgerSummary.Categories) != 0 {
			t.Fatalf("BE-14 城市回忆抽屉访客泄露财务数据: %+v", drawerResp.Data.LedgerSummary)
		}

		// 管理员视角
		wAdmin := httptest.NewRecorder()
		reqAdmin, _ := http.NewRequest(http.MethodGet, "/api/v1/footprints/cities/SHA/drawer", nil)
		reqAdmin.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(wAdmin, reqAdmin)
		if wAdmin.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", wAdmin.Code)
		}
		var adminDrawerResp struct {
			Data struct {
				LedgerSummary struct {
					TotalSpent       int64         `json:"total_spent"`
					TransactionCount int           `json:"transaction_count"`
					Categories       []interface{} `json:"categories"`
				} `json:"ledger_summary"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wAdmin.Body.Bytes(), &adminDrawerResp)
		if adminDrawerResp.Data.LedgerSummary.TotalSpent != 12800 ||
			adminDrawerResp.Data.LedgerSummary.TransactionCount != 1 {
			t.Fatalf("BE-14 管理员城市回忆抽屉数据错误: %+v", adminDrawerResp.Data.LedgerSummary)
		}
	})

	// -------------------------------------------------------------
	// C. 检验年度生活年鉴脱敏: GET /api/v1/almanac/summary?year=2026
	// -------------------------------------------------------------
	t.Run("Almanac Visitor Desensitization", func(t *testing.T) {
		// 访客视角
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/almanac/summary?year=2026", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var almanacResp struct {
			Data struct {
				Prologue struct {
					TxCount int `json:"tx_count"`
				} `json:"prologue"`
				WishesMilestones struct {
					TotalWishActual int64         `json:"total_wish_actual"`
					FulfilledWishes []models.Wish `json:"fulfilled_wishes"`
				} `json:"wishes_milestones"`
				FinancialSpectrum struct {
					TotalIncome  int64 `json:"total_income"`
					TotalExpense int64 `json:"total_expense"`
					Balance      int64 `json:"balance"`
				} `json:"financial_spectrum"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &almanacResp)

		if almanacResp.Data.Prologue.TxCount != 0 {
			t.Fatalf("BE-14 年鉴访客模式 Prologue 泄露 tx_count: %d", almanacResp.Data.Prologue.TxCount)
		}
		if almanacResp.Data.WishesMilestones.TotalWishActual != 0 {
			t.Fatalf("BE-14 年鉴访客模式 Milestones 泄露 total_wish_actual: %d", almanacResp.Data.WishesMilestones.TotalWishActual)
		}
		for _, wItem := range almanacResp.Data.WishesMilestones.FulfilledWishes {
			if wItem.ActualCost != 0 || wItem.TransactionID != nil || wItem.GearID != nil {
				t.Fatalf("BE-14 年鉴访客模式心愿明细泄露财务: actual_cost=%d", wItem.ActualCost)
			}
		}
		if almanacResp.Data.FinancialSpectrum.TotalIncome != 0 ||
			almanacResp.Data.FinancialSpectrum.TotalExpense != 0 ||
			almanacResp.Data.FinancialSpectrum.Balance != 0 {
			t.Fatalf("BE-14 年鉴访客模式 FinancialSpectrum 泄露金额: %+v", almanacResp.Data.FinancialSpectrum)
		}

		// 管理员视角
		wAdmin := httptest.NewRecorder()
		reqAdmin, _ := http.NewRequest(http.MethodGet, "/api/v1/almanac/summary?year=2026", nil)
		reqAdmin.Header.Set("Authorization", "Bearer "+testMasterKey)
		r.ServeHTTP(wAdmin, reqAdmin)
		if wAdmin.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", wAdmin.Code)
		}
		var adminAlmanacResp struct {
			Data struct {
				Prologue struct {
					TxCount int `json:"tx_count"`
				} `json:"prologue"`
				FinancialSpectrum struct {
					TotalIncome  int64 `json:"total_income"`
					TotalExpense int64 `json:"total_expense"`
					Balance      int64 `json:"balance"`
				} `json:"financial_spectrum"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wAdmin.Body.Bytes(), &adminAlmanacResp)
		if adminAlmanacResp.Data.Prologue.TxCount == 0 {
			t.Fatalf("BE-14 管理员年鉴 tx_count 应大于 0")
		}
		if adminAlmanacResp.Data.FinancialSpectrum.TotalIncome != 80000 ||
			adminAlmanacResp.Data.FinancialSpectrum.TotalExpense != 12800 {
			t.Fatalf("BE-14 管理员年鉴财务金额不正确: %+v", adminAlmanacResp.Data.FinancialSpectrum)
		}
	})
}

// TestBE15_InitialLiquidBalanceAndAssetsOverview 测试初始流动资产基准与总览核算 (BE-15)
func TestBE15_InitialLiquidBalanceAndAssetsOverview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	// 创建主理人档案
	db.DB.Create(&models.CuratorProfile{Name: "测试主理人"})

	testMasterKey := "test-secret-be15"
	config.AppConfig.AdminAPIKey = testMasterKey
	defer func() {
		config.AppConfig.AdminAPIKey = ""
	}()

	r := gin.New()
	v1 := r.Group("/api/v1")
	write := v1.Group("")
	write.Use(middleware.AdminAuthRequired())
	{
		write.GET("/assets/overview", GetAssetsOverview)
		write.PUT("/assets/liquid-balance", SetInitialLiquidBalance)
	}

	// 1. 设置初始流动资产基准: 50,000 元 (5,000,000 分)
	initialBalance := int64(5000000)
	payload := SetLiquidBalanceRequest{InitialLiquidBalance: initialBalance}
	body, _ := json.Marshal(payload)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/assets/liquid-balance", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+testMasterKey)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on SetInitialLiquidBalance, got %d: %s", w.Code, w.Body.String())
	}

	// 验证数据库持久化
	var cfg models.SiteConfig
	db.DB.First(&cfg)
	if cfg.InitialLiquidBalance != initialBalance {
		t.Fatalf("expected DB InitialLiquidBalance = %d, got %d", initialBalance, cfg.InitialLiquidBalance)
	}

	// 2. 插入一笔收入和一笔支出流水
	db.DB.Create(&models.Transaction{
		Amount:     100000, // +1,000 元
		Type:       "income",
		Category:   "工资",
		Title:      "兼职稿酬",
		OccurredAt: time.Now(),
	})
	db.DB.Create(&models.Transaction{
		Amount:     20000, // -200 元
		Type:       "expense",
		Category:   "日常餐饮",
		Title:      "周末聚餐",
		OccurredAt: time.Now(),
	})

	// 3. 获取资产总览并核算可用流动资产 (BE-15)
	// 期望 liquid_balance = 5,000,000 + 100,000 - 20,000 = 5,080,000 分
	wOverview := httptest.NewRecorder()
	reqOverview, _ := http.NewRequest(http.MethodGet, "/api/v1/assets/overview", nil)
	reqOverview.Header.Set("Authorization", "Bearer "+testMasterKey)
	r.ServeHTTP(wOverview, reqOverview)

	if wOverview.Code != http.StatusOK {
		t.Fatalf("expected 200 on GetAssetsOverview, got %d", wOverview.Code)
	}

	var overviewResp struct {
		Data AssetsOverviewResponse `json:"data"`
	}
	_ = json.Unmarshal(wOverview.Body.Bytes(), &overviewResp)

	expectedLiquid := int64(5080000)
	if overviewResp.Data.InitialLiquidBalance != initialBalance {
		t.Fatalf("expected initial_liquid_balance=%d, got %d", initialBalance, overviewResp.Data.InitialLiquidBalance)
	}
	if overviewResp.Data.LiquidBalance != expectedLiquid {
		t.Fatalf("expected liquid_balance=%d, got %d", expectedLiquid, overviewResp.Data.LiquidBalance)
	}
	if overviewResp.Data.NetWorth != expectedLiquid {
		t.Fatalf("expected net_worth=%d, got %d", expectedLiquid, overviewResp.Data.NetWorth)
	}
}
