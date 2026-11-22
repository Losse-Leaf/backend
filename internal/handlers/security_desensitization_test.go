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

// TestBE13_AuthenticationBypassPrevention 测试致命级安全鉴权穿透修复 (BE-13, BE-18)
// 任何未携带有效 active 设备令牌的请求访问受保护接口，必须严格返回 401
func TestBE13_AuthenticationBypassPrevention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	// 1. 初始化站点配置
	var cfg models.SiteConfig
	db.DB.FirstOrCreate(&cfg)
	cfg.Initialized = true
	db.DB.Save(&cfg)

	r := gin.New()
	v1 := r.Group("/api/v1")
	protected := v1.Group("")
	protected.Use(middleware.AdminAuthRequired())
	{
		protected.GET("/transactions", GetTransactions)
		protected.POST("/transactions", CreateTransaction)
	}

	// 访客未带 Token 访问受保护接口必须被拦截 (401)
	endpoints := []string{"/api/v1/transactions"}
	for _, ep := range endpoints {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, ep, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("BE-13 漏洞暴露: ep %s expected 401 Unauthorized, got %d: %s", ep, w.Code, w.Body.String())
		}
	}

	// 访客未带 Token 访问 POST 接口必须被拦截 (401)
	wPost := httptest.NewRecorder()
	reqPost, _ := http.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewBufferString(`{"amount": 100, "category": "餐饮"}`))
	reqPost.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wPost, reqPost)
	if wPost.Code != http.StatusUnauthorized {
		t.Fatalf("BE-13 漏洞暴露: POST /transactions expected 401, got %d", wPost.Code)
	}
}

func assertEmbeddedWishRedacted(t *testing.T, entries []models.LifeEntry) {
	t.Helper()
	for _, entry := range entries {
		if entry.Wish == nil {
			continue
		}
		if entry.Wish.ActualCost != 0 || entry.Wish.TransactionID != nil {
			t.Fatalf("访客响应中的嵌套心愿泄露财务字段: actual_cost=%d tx=%v",
				entry.Wish.ActualCost, entry.Wish.TransactionID)
		}
		return
	}
	t.Fatal("测试夹具应包含一条嵌套心愿的散页")
}

func assertEmbeddedWishVisible(t *testing.T, entries []models.LifeEntry) {
	t.Helper()
	for _, entry := range entries {
		if entry.Wish == nil {
			continue
		}
		if entry.Wish.ActualCost != 1250000 || entry.Wish.TransactionID == nil {
			t.Fatalf("管理员响应应保留嵌套心愿完整财务: actual_cost=%d tx=%v",
				entry.Wish.ActualCost, entry.Wish.TransactionID)
		}
		return
	}
	t.Fatal("测试夹具应包含一条嵌套心愿的散页")
}

// TestBE14_PublicVisitorFinancialDesensitization 测试公开接口财务隐私脱敏 (BE-14)
// 心愿单、年度生活年鉴公开只读接口在访客模式下必须严格脱敏
func TestBE14_PublicVisitorFinancialDesensitization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	enableAllPublicModulesForTest(t)

	testMasterKey := "lldt_admin_secret_999"
	seedTestAdminDevice(testMasterKey)

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
	fulfilledWish := models.Wish{
		Title:          "富士 X100VI 相机",
		Kind:           "item",
		Budget:         1199900,
		ActualCost:     1250000,
		Status:         "fulfilled",
		ResolutionType: "exact",
		TransactionID:  &txID,
		FulfilledAt:    &txExpense.OccurredAt,
	}
	db.DB.Create(&fulfilledWish)

	// 2b. 关联心愿的散页：嵌套心愿财务字段曾绕过年鉴高光的脱敏
	wishEntry := models.LifeEntry{
		Type: "life", Title: "心愿达成记", Content: "在上海把这台相机带回了家",
		Location:   "上海市",
		OccurredAt: time.Date(2026, 5, 20, 18, 0, 0, 0, config.BeijingLocation),
		WishID:     &fulfilledWish.ID,
	}
	db.DB.Create(&wishEntry)

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
		if item.ActualCost != 0 || item.TransactionID != nil {
			t.Fatalf("BE-14 访客列表泄露财务/关联隐私: actual_cost=%d, txID=%v",
				item.ActualCost, item.TransactionID)
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
		if detailResp.Data.ActualCost != 0 || detailResp.Data.TransactionID != nil {
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
		if adminResp.Data.ActualCost != 1250000 || adminResp.Data.TransactionID == nil {
			t.Fatalf("BE-14 管理员未获取到完整财务数据: actual_cost=%d", adminResp.Data.ActualCost)
		}
	})

	// -------------------------------------------------------------
	// B. 检验年度生活年鉴脱敏: GET /api/v1/almanac/summary?year=2026
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
				Wishes struct {
					TotalActual int64         `json:"total_actual"`
					Wishes      []models.Wish `json:"wishes"`
				} `json:"wishes"`
				Highlights struct {
					Spring *models.LifeEntry `json:"spring"`
					Summer *models.LifeEntry `json:"summer"`
					Autumn *models.LifeEntry `json:"autumn"`
					Winter *models.LifeEntry `json:"winter"`
				} `json:"highlights"`
				Finances struct {
					TotalIncome  int64 `json:"total_income"`
					TotalExpense int64 `json:"total_expense"`
					Balance      int64 `json:"balance"`
				} `json:"finances"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &almanacResp)

		if almanacResp.Data.Prologue.TxCount != 0 {
			t.Fatalf("BE-14 年鉴访客模式 Prologue 泄露 tx_count: %d", almanacResp.Data.Prologue.TxCount)
		}
		var highlightEntries []models.LifeEntry
		for _, highlight := range []*models.LifeEntry{
			almanacResp.Data.Highlights.Spring, almanacResp.Data.Highlights.Summer,
			almanacResp.Data.Highlights.Autumn, almanacResp.Data.Highlights.Winter,
		} {
			if highlight != nil {
				highlightEntries = append(highlightEntries, *highlight)
			}
		}
		assertEmbeddedWishRedacted(t, highlightEntries)
		if almanacResp.Data.Wishes.TotalActual != 0 {
			t.Fatalf("BE-14 年鉴访客模式 Wishes 泄露 total_actual: %d", almanacResp.Data.Wishes.TotalActual)
		}
		for _, wItem := range almanacResp.Data.Wishes.Wishes {
			if wItem.ActualCost != 0 || wItem.TransactionID != nil {
				t.Fatalf("BE-14 年鉴访客模式心愿明细泄露财务: actual_cost=%d", wItem.ActualCost)
			}
		}
		if almanacResp.Data.Finances.TotalIncome != 0 ||
			almanacResp.Data.Finances.TotalExpense != 0 ||
			almanacResp.Data.Finances.Balance != 0 {
			t.Fatalf("BE-14 年鉴访客模式 Finances 泄露金额: %+v", almanacResp.Data.Finances)
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
				Highlights struct {
					Spring *models.LifeEntry `json:"spring"`
					Summer *models.LifeEntry `json:"summer"`
					Autumn *models.LifeEntry `json:"autumn"`
					Winter *models.LifeEntry `json:"winter"`
				} `json:"highlights"`
				Finances struct {
					TotalIncome  int64 `json:"total_income"`
					TotalExpense int64 `json:"total_expense"`
					Balance      int64 `json:"balance"`
				} `json:"finances"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wAdmin.Body.Bytes(), &adminAlmanacResp)
		var adminHighlights []models.LifeEntry
		for _, highlight := range []*models.LifeEntry{
			adminAlmanacResp.Data.Highlights.Spring, adminAlmanacResp.Data.Highlights.Summer,
			adminAlmanacResp.Data.Highlights.Autumn, adminAlmanacResp.Data.Highlights.Winter,
		} {
			if highlight != nil {
				adminHighlights = append(adminHighlights, *highlight)
			}
		}
		assertEmbeddedWishVisible(t, adminHighlights)
		if adminAlmanacResp.Data.Prologue.TxCount == 0 {
			t.Fatalf("BE-14 管理员年鉴 tx_count 应大于 0")
		}
		if adminAlmanacResp.Data.Finances.TotalIncome != 80000 ||
			adminAlmanacResp.Data.Finances.TotalExpense != 12800 {
			t.Fatalf("BE-14 管理员年鉴财务金额不正确: %+v", adminAlmanacResp.Data.Finances)
		}
	})
}

// TestBE17_ETagRoleCacheIsolation 测试心愿单与年鉴 ETag 跨身份缓存隔离及 RG-17 双键兼容 (BE-17, RG-17)
func TestBE17_ETagRoleCacheIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	enableAllPublicModulesForTest(t)

	testMasterKey := "lldt_secret_be17"
	seedTestAdminDevice(testMasterKey)

	var cfg models.SiteConfig
	db.DB.FirstOrCreate(&cfg)
	cfg.Initialized = true
	db.DB.Save(&cfg)

	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.Local)
	db.DB.Create(&models.Wish{
		Title:       "Sony A7C II",
		Kind:        "item",
		Budget:      1399900,
		ActualCost:  1350000,
		Status:      "fulfilled",
		FulfilledAt: &now,
	})
	db.DB.Create(&models.Transaction{
		Amount:     1350000,
		Type:       "expense",
		Category:   "好物购置",
		Title:      "购置相机",
		OccurredAt: now,
	})

	r := gin.New()
	r.GET("/api/v1/wishes", GetWishes)
	r.GET("/api/v1/almanac/summary", GetAnnualAlmanacSummary)

	// 1. 访客先请求 /api/v1/wishes 获取 guest ETag
	wGuestWish := httptest.NewRecorder()
	reqGuestWish, _ := http.NewRequest(http.MethodGet, "/api/v1/wishes", nil)
	r.ServeHTTP(wGuestWish, reqGuestWish)
	guestWishETag := wGuestWish.Header().Get("ETag")
	if guestWishETag == "" {
		t.Fatalf("expected non-empty ETag for guest wishes")
	}

	// 2. 携带 guest ETag 但切换为 Admin Token 请求 -> 必须返回 200 OK 而非 304!
	wAdminWish := httptest.NewRecorder()
	reqAdminWish, _ := http.NewRequest(http.MethodGet, "/api/v1/wishes", nil)
	reqAdminWish.Header.Set("If-None-Match", guestWishETag)
	reqAdminWish.Header.Set("Authorization", "Bearer "+testMasterKey)
	r.ServeHTTP(wAdminWish, reqAdminWish)
	if wAdminWish.Code != http.StatusOK {
		t.Fatalf("BE-17 跨权限缓存污染: expected 200 OK when switching guest->admin, got %d", wAdminWish.Code)
	}
	adminWishETag := wAdminWish.Header().Get("ETag")
	if adminWishETag == guestWishETag {
		t.Fatalf("BE-17 admin ETag (%s) must differ from guest ETag (%s)", adminWishETag, guestWishETag)
	}

	// 3. 访客请求 /api/v1/almanac/summary 获取 guest ETag，切换 Admin 后必须返回 200 OK 且兼容 RG-17 字段
	wGuestAlm := httptest.NewRecorder()
	reqGuestAlm, _ := http.NewRequest(http.MethodGet, "/api/v1/almanac/summary?year=2026", nil)
	r.ServeHTTP(wGuestAlm, reqGuestAlm)
	guestAlmETag := wGuestAlm.Header().Get("ETag")

	wAdminAlm := httptest.NewRecorder()
	reqAdminAlm, _ := http.NewRequest(http.MethodGet, "/api/v1/almanac/summary?year=2026", nil)
	reqAdminAlm.Header.Set("If-None-Match", guestAlmETag)
	reqAdminAlm.Header.Set("Authorization", "Bearer "+testMasterKey)
	r.ServeHTTP(wAdminAlm, reqAdminAlm)
	if wAdminAlm.Code != http.StatusOK {
		t.Fatalf("BE-17 年鉴跨权限缓存污染: expected 200 OK when switching guest->admin, got %d", wAdminAlm.Code)
	}

	var almBody struct {
		Data map[string]interface{} `json:"data"`
	}
	_ = json.Unmarshal(wAdminAlm.Body.Bytes(), &almBody)
	// 验证年鉴统一规范字段
	for _, key := range []string{"wishes", "highlights", "finances"} {
		if _, exists := almBody.Data[key]; !exists {
			t.Fatalf("年鉴响应缺失字段: %s", key)
		}
	}
}

// TestGlobalStatsRoleCacheIsolation 测试全局统计接口对访客隐藏当月支出且不污染主理人缓存
func TestGlobalStatsRoleCacheIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	InvalidateStatsCache()

	testMasterKey := "lldt_secret_stats_isolation"
	seedTestAdminDevice(testMasterKey)

	db.DB.Create(&models.Transaction{
		Amount:     88800,
		Type:       "expense",
		Category:   "日常餐饮",
		Title:      "测试支出",
		OccurredAt: time.Now().In(config.BeijingLocation),
	})

	r := gin.New()
	r.GET("/api/v1/stats", GetGlobalStats)

	// 1. 访客先请求，month_expense 必须为 0
	wGuest := httptest.NewRecorder()
	reqGuest, _ := http.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	r.ServeHTTP(wGuest, reqGuest)
	if wGuest.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wGuest.Code)
	}
	var guestResp struct {
		Data map[string]interface{} `json:"data"`
	}
	_ = json.Unmarshal(wGuest.Body.Bytes(), &guestResp)
	if int64(guestResp.Data["month_expense"].(float64)) != 0 {
		t.Fatalf("expected month_expense=0 for guest, got %v", guestResp.Data["month_expense"])
	}

	// 2. 主理人命中缓存请求，必须拿到真实 month_expense=88800
	wAdmin := httptest.NewRecorder()
	reqAdmin, _ := http.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	reqAdmin.Header.Set("Authorization", "Bearer "+testMasterKey)
	r.ServeHTTP(wAdmin, reqAdmin)
	if wAdmin.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wAdmin.Code)
	}
	var adminResp struct {
		Data map[string]interface{} `json:"data"`
	}
	_ = json.Unmarshal(wAdmin.Body.Bytes(), &adminResp)
	if int64(adminResp.Data["month_expense"].(float64)) != 88800 {
		t.Fatalf("expected month_expense=88800 for admin, got %v", adminResp.Data["month_expense"])
	}

	// 3. 访客再次命中缓存请求，仍必须为 0
	wGuest2 := httptest.NewRecorder()
	reqGuest2, _ := http.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	r.ServeHTTP(wGuest2, reqGuest2)
	_ = json.Unmarshal(wGuest2.Body.Bytes(), &guestResp)
	if int64(guestResp.Data["month_expense"].(float64)) != 0 {
		t.Fatalf("expected month_expense=0 for guest after admin cache hit, got %v", guestResp.Data["month_expense"])
	}
}
