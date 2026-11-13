package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
)

func enableAllPublicModulesForTest(t *testing.T) {
	t.Helper()
	var cfg models.SiteConfig
	db.DB.FirstOrCreate(&cfg)
	cfg.PublicModulesRaw = MarshalPublicModulesRaw(AllowedPublicModules)
	if err := db.DB.Save(&cfg).Error; err != nil {
		t.Fatalf("failed to enable public modules for test: %v", err)
	}
	InvalidatePublicModulesCache()
	t.Cleanup(InvalidatePublicModulesCache)
}

func TestPrivacyV2_DefaultAllPrivateAndModuleToggles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	InvalidatePublicModulesCache()
	t.Cleanup(InvalidatePublicModulesCache)

	// 创建 1 条公开散页、1 条私密散页、1 个公开造物、1 个私密造物
	db.DB.Create(&models.LifeEntry{Type: "life", Title: "Public Entry", Content: "hello public", OccurredAt: time.Now(), IsPrivate: false})
	db.DB.Create(&models.LifeEntry{Type: "life", Title: "Secret Entry", Content: "hello secret", OccurredAt: time.Now(), IsPrivate: true})
	db.DB.Create(&models.Project{Title: "Public Project", Description: "open", Status: "进行中", IsPrivate: false})
	db.DB.Create(&models.Project{Title: "Secret Project", Description: "hidden", Status: "进行中", IsPrivate: true})
	db.DB.Create(&models.Wish{Title: "Public Wish", Kind: "item", Status: "wanting", IsPrivate: false})
	db.DB.Create(&models.Wish{Title: "Secret Wish", Kind: "item", Status: "wanting", IsPrivate: true})
	db.DB.Create(&models.Footprint{CityCode: "330100", CityName: "杭州市", Title: "Public Trip", VisitedAt: time.Now(), IsPrivate: false})
	db.DB.Create(&models.Footprint{CityCode: "110100", CityName: "北京市", Title: "Secret Trip", VisitedAt: time.Now(), IsPrivate: true})
	db.DB.Create(&models.CuratorProfile{Name: "主理人", Title: "档案官"})

	adminToken := "lldt_privacy_token_1"
	adminToken2 := "lldt_privacy_token_2"
	db.DB.Create(&models.Device{
		DeviceID:     "dev_web_privacy_1",
		Name:         "Web Test 1",
		Platform:     "web",
		TokenHash:    middleware.HashToken(adminToken),
		Status:       "active",
		PairedAt:     time.Now(),
		LastActiveAt: time.Now(),
	})
	db.DB.Create(&models.Device{
		DeviceID:     "dev_mac_privacy_2",
		Name:         "Mac Test 2",
		Platform:     "macos",
		TokenHash:    middleware.HashToken(adminToken2),
		Status:       "active",
		PairedAt:     time.Now(),
		LastActiveAt: time.Now(),
	})

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		v1.GET("/config", GetConfigV1)
		v1.GET("/entries", GetLifeEntriesV1)
		v1.GET("/entries/:id", GetLifeEntryByID)
		v1.GET("/projects", GetProjectsV1)
		v1.GET("/projects/:id", GetProjectByID)
		v1.GET("/wishes", GetWishes)
		v1.GET("/wishes/:id", GetWishByID)
		v1.GET("/footprints", GetFootprints)
		v1.GET("/footprints/atlas", GetFootprintsAtlas)
		v1.GET("/footprints/:id", GetFootprintByID)
		v1.GET("/profile", GetCuratorProfile)
		v1.GET("/almanac/summary", GetAnnualAlmanacSummary)
		v1.GET("/stats", GetGlobalStats)
		v1.GET("/search", GlobalSearch)
	}
	write := v1.Group("")
	write.Use(middleware.AdminAuthRequired())
	{
		write.PUT("/config", UpdateConfig)
		write.GET("/auth/devices", GetTrustedDevices)
	}

	// 1. 默认全站私有：所有展厅接口对未登录访客返回 403
	for _, path := range []string{
		"/api/v1/entries",
		"/api/v1/projects",
		"/api/v1/wishes",
		"/api/v1/footprints",
		"/api/v1/footprints/atlas",
		"/api/v1/profile",
		"/api/v1/almanac/summary",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for %s when site is private, got %d", path, w.Code)
		}
	}

	// 访客 /stats 全为 0，/search 全为空
	wStats := httptest.NewRecorder()
	r.ServeHTTP(wStats, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil))
	if wStats.Code != http.StatusOK {
		t.Fatalf("expected 200 for /stats, got %d", wStats.Code)
	}
	var statsBody struct {
		Data struct {
			TotalEntries  int `json:"total_entries"`
			TotalProjects int `json:"total_projects"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wStats.Body.Bytes(), &statsBody)
	if statsBody.Data.TotalEntries != 0 || statsBody.Data.TotalProjects != 0 {
		t.Fatalf("expected 0 public stats when modules disabled, got %+v", statsBody.Data)
	}

	wSearch := httptest.NewRecorder()
	r.ServeHTTP(wSearch, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=Public", nil))
	var searchBody struct {
		Data struct {
			Entries  []models.LifeEntry `json:"entries"`
			Projects []models.Project   `json:"projects"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wSearch.Body.Bytes(), &searchBody)
	if len(searchBody.Data.Entries) != 0 || len(searchBody.Data.Projects) != 0 {
		t.Fatalf("expected empty search results when modules disabled, got %+v", searchBody.Data)
	}

	// 2. 通过 PUT /config 开启 entries 与 projects 公开（故意带一个非法模块名验证白名单过滤）
	cfgPayload := []byte(`{"public_modules":["entries","projects","transactions","invalid"]}`)
	reqCfg := httptest.NewRequest(http.MethodPut, "/api/v1/config", bytes.NewReader(cfgPayload))
	reqCfg.Header.Set("Content-Type", "application/json")
	reqCfg.Header.Set("X-Device-Token", adminToken)
	wCfg := httptest.NewRecorder()
	r.ServeHTTP(wCfg, reqCfg)
	if wCfg.Code != http.StatusOK {
		t.Fatalf("PUT /config failed: %d %s", wCfg.Code, wCfg.Body.String())
	}

	// 访客拉取 /entries 只看到非私密条目
	wEntries := httptest.NewRecorder()
	r.ServeHTTP(wEntries, httptest.NewRequest(http.MethodGet, "/api/v1/entries", nil))
	if wEntries.Code != http.StatusOK {
		t.Fatalf("expected 200 for /entries after enabling module, got %d", wEntries.Code)
	}
	var entriesResp struct {
		Data struct {
			Entries []models.LifeEntry `json:"entries"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wEntries.Body.Bytes(), &entriesResp)
	if len(entriesResp.Data.Entries) != 1 || entriesResp.Data.Entries[0].Title != "Public Entry" {
		t.Fatalf("expected only 1 public entry for visitor, got %+v", entriesResp.Data.Entries)
	}

	// 访客直接访问私密条目详情返回 404
	wSecretEntry := httptest.NewRecorder()
	r.ServeHTTP(wSecretEntry, httptest.NewRequest(http.MethodGet, "/api/v1/entries/2", nil))
	if wSecretEntry.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for private entry detail, got %d", wSecretEntry.Code)
	}

	// 管理员访问 /entries 可以看到全部 2 条
	reqAdminEntries := httptest.NewRequest(http.MethodGet, "/api/v1/entries", nil)
	reqAdminEntries.Header.Set("X-Device-Token", adminToken)
	wAdminEntries := httptest.NewRecorder()
	r.ServeHTTP(wAdminEntries, reqAdminEntries)
	var adminEntriesResp struct {
		Data struct {
			Entries []models.LifeEntry `json:"entries"`
		} `json:"data"`
	}
	_ = json.Unmarshal(wAdminEntries.Body.Bytes(), &adminEntriesResp)
	if len(adminEntriesResp.Data.Entries) != 2 {
		t.Fatalf("expected 2 entries for admin, got %d", len(adminEntriesResp.Data.Entries))
	}

	// 未开启的 wishes 依然 403
	wWishes := httptest.NewRecorder()
	r.ServeHTTP(wWishes, httptest.NewRequest(http.MethodGet, "/api/v1/wishes", nil))
	if wWishes.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled wishes module, got %d", wWishes.Code)
	}

	// 3. 验证 Device.is_current 仅标记当前调用设备
	reqDev := httptest.NewRequest(http.MethodGet, "/api/v1/auth/devices", nil)
	reqDev.Header.Set("X-Device-Token", adminToken)
	wDev := httptest.NewRecorder()
	r.ServeHTTP(wDev, reqDev)
	if wDev.Code != http.StatusOK {
		t.Fatalf("GET /auth/devices failed: %d", wDev.Code)
	}
	var devResp struct {
		Data []models.Device `json:"data"`
	}
	_ = json.Unmarshal(wDev.Body.Bytes(), &devResp)
	if len(devResp.Data) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devResp.Data))
	}
	currentCount := 0
	for _, d := range devResp.Data {
		if d.IsCurrent {
			currentCount++
			if d.DeviceID != "dev_web_privacy_1" {
				t.Fatalf("expected dev_web_privacy_1 to be current, got %s", d.DeviceID)
			}
		}
	}
	if currentCount != 1 {
		t.Fatalf("expected exactly 1 current device, got %d", currentCount)
	}
}
