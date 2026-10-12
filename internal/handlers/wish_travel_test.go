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
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

func TestWishAndTravelModules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		v1.GET("/footprints", GetFootprints)
		v1.GET("/footprints/atlas", GetFootprintsAtlas)
		v1.GET("/footprints/provinces/:adcode/drawer", GetProvinceMemoryDrawer)
		v1.GET("/footprints/cities/:city_code/drawer", GetCityMemoryDrawer)
		v1.GET("/footprints/:id", GetFootprintByID)
		v1.POST("/footprints", CreateFootprint)
		v1.PUT("/footprints/:id", UpdateFootprint)
		v1.DELETE("/footprints/:id", DeleteFootprint)

		v1.GET("/wishes", GetWishes)
		v1.GET("/wishes/:id", GetWishByID)
		v1.POST("/wishes", CreateWish)
		v1.PUT("/wishes/:id", UpdateWish)
		v1.POST("/wishes/:id/resolve", ResolveWish)
		v1.DELETE("/wishes/:id", DeleteWish)

		v1.GET("/almanac/summary", GetAnnualAlmanacSummary)
	}

	// 1. Footprint CRUD, Atlas & Drawer
	t.Run("Footprints CRUD and Atlas", func(t *testing.T) {
		fpReq := CreateFootprintRequest{
			CityCode:  "330100",
			CityName:  "杭州市",
			VisitedAt: "2026-10-08 14:00:00",
			Note:      "春日西湖漫步",
		}
		body, _ := json.Marshal(fpReq)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/footprints", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create footprint, got %d: %s", w.Code, w.Body.String())
		}

		// Atlas endpoint
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/footprints/atlas", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on atlas, got %d", w.Code)
		}
		var atlasResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &atlasResp)
		data := atlasResp["data"].(map[string]interface{})
		if int(data["total_cities"].(float64)) != 1 {
			t.Fatalf("expected 1 city in atlas, got %v", data["total_cities"])
		}

		// City Drawer endpoint
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/footprints/cities/330100/drawer", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on drawer, got %d", w.Code)
		}
		var drawerResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &drawerResp)
		drawerData := drawerResp["data"].(map[string]interface{})
		if drawerData["city_name"] != "杭州市" {
			t.Fatalf("expected 杭州市 in drawer, got %v", drawerData["city_name"])
		}

		// Add another city in Zhejiang (Ningbo 330200) to test Province Drawer (RC-91)
		fpReq2 := CreateFootprintRequest{
			CityCode:  "330200",
			CityName:  "宁波市",
			VisitedAt: "2026-10-09 10:00:00",
			Note:      "老外滩夜景",
		}
		body2, _ := json.Marshal(fpReq2)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, "/api/v1/footprints", bytes.NewBuffer(body2))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create footprint 2, got %d", w.Code)
		}

		// Province Drawer endpoint (GET /api/v1/footprints/provinces/330000/drawer)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, "/api/v1/footprints/provinces/330000/drawer", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on province drawer, got %d", w.Code)
		}
		var provResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &provResp)
		provData := provResp["data"].(map[string]interface{})
		if provData["province_adcode"] != "330000" {
			t.Fatalf("expected province_adcode 330000, got %v", provData["province_adcode"])
		}
		if provData["province_name"] != "浙江省" {
			t.Fatalf("expected province_name 浙江省, got %v", provData["province_name"])
		}
		visitedCities := provData["visited_cities"].([]interface{})
		if len(visitedCities) != 2 {
			t.Fatalf("expected 2 visited cities in Zhejiang, got %d", len(visitedCities))
		}
		visitStats := provData["visit_stats"].(map[string]interface{})
		if int(visitStats["city_count"].(float64)) != 2 {
			t.Fatalf("expected city_count 2, got %v", visitStats["city_count"])
		}
		if int(visitStats["total_visits"].(float64)) != 2 {
			t.Fatalf("expected total_visits 2, got %v", visitStats["total_visits"])
		}
	})

	// 2. Wish Creation and Resolve with Multi-Module Linkage
	t.Run("Wish Creation and Resolve Ritual Linkage", func(t *testing.T) {
		wishReq := CreateWishRequest{
			Title:      "云南大理旅居两周",
			Kind:       "travel",
			CityCode:   "532901",
			CityName:   "大理市",
			Budget:     600000,
			Note:       "吹洱海的风，看苍山的雪",
			TargetDate: "2026-10-01",
		}
		body, _ := json.Marshal(wishReq)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/wishes", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on create wish, got %d", w.Code)
		}

		var createResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &createResp)
		wishData := createResp["data"].(map[string]interface{})
		wishID := int(wishData["id"].(float64))

		// Resolve ritual
		resolveReq := ResolveWishRequest{
			FulfilledAt:       time.Now().Format(time.RFC3339),
			ActualCost:        520000,
			CreateTransaction: true,
			CreateLifeEntry:   true,
			CreateFootprint:   true,
			EntryContent:      "苍山雪冷，洱海天阔，完成了久违的大理漫游。",
		}
		resBody, _ := json.Marshal(resolveReq)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/wishes/%d/resolve", wishID), bytes.NewBuffer(resBody))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on resolve wish, got %d: %s", w.Code, w.Body.String())
		}

		var resResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &resResp)
		resolvedWish := resResp["data"].(map[string]interface{})
		if resolvedWish["status"] != "fulfilled" {
			t.Fatalf("expected status fulfilled, got %v", resolvedWish["status"])
		}
		if resolvedWish["transaction_id"] == nil || resolvedWish["life_entry_id"] == nil || resolvedWish["footprint_id"] == nil {
			t.Fatalf("expected all 3 linkages to be non-nil: %+v", resolvedWish)
		}
	})

	// 3. Annual Almanac Summary
	t.Run("Annual Almanac Summary", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/almanac/summary?year=2026", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on almanac, got %d", w.Code)
		}
		var almanacResp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &almanacResp)
		data := almanacResp["data"].(map[string]interface{})
		if int(data["year"].(float64)) != 2026 {
			t.Fatalf("expected year 2026 in almanac")
		}
		prologue := data["prologue"].(map[string]interface{})
		if int(prologue["wishes_fulfilled"].(float64)) < 1 {
			t.Fatalf("expected at least 1 fulfilled wish in almanac")
		}
	})

	// 4. Record-First Architecture: Companion Creation & Natural Infiltration (docs/wishlist-travel.md §6 & §11)
	t.Run("Record-First Companion Creation and Natural Infiltration", func(t *testing.T) {
		r.POST("/api/v1/entries", CreateLifeEntry)
		r.PUT("/api/v1/entries/:id", UpdateLifeEntry)
		r.DELETE("/api/v1/entries/:id", DeleteLifeEntry)
		r.GET("/api/v1/entries", GetLifeEntriesV1)
		r.POST("/api/v1/projects", CreateProject)
		r.PUT("/api/v1/projects/:id", UpdateProject)
		r.GET("/api/v1/projects/:id", GetProjectByID)

		// (a) 2.1 足迹录入伴生写入时间流：录入雅安足迹 (511800) 自动伴生创建 LifeEntry
		fpYaan := CreateFootprintRequest{
			CityCode:  "511800",
			CityName:  "雅安市",
			VisitedAt: "2026-10-11 09:30:00",
			Note:      "碧峰峡看大熊猫，蒙顶山品秋茶",
		}
		fpBody, _ := json.Marshal(fpYaan)
		wFP := httptest.NewRecorder()
		reqFP, _ := http.NewRequest(http.MethodPost, "/api/v1/footprints", bytes.NewBuffer(fpBody))
		reqFP.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wFP, reqFP)
		if wFP.Code != http.StatusOK {
			t.Fatalf("expected 200 creating footprint, got %d: %s", wFP.Code, wFP.Body.String())
		}
		var fpResp struct {
			Data struct {
				ID          uint  `json:"id"`
				LifeEntryID *uint `json:"life_entry_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wFP.Body.Bytes(), &fpResp)
		if fpResp.Data.LifeEntryID == nil || *fpResp.Data.LifeEntryID == 0 {
			t.Fatalf("expected CreateFootprint to automatically create companion LifeEntry and populate life_entry_id")
		}

		// (b) 2.2 造物流水伴生写入时间流：新建造物 & 更新进展自动伴生创建 LifeEntry
		projCreate := ProjectRequest{
			Title:       "手工胡桃木托盘",
			Description: "打磨一块温润的桌面置物木盘",
			Category:    "手艺造物",
			Status:      "Active",
		}
		projBody, _ := json.Marshal(projCreate)
		wProj := httptest.NewRecorder()
		reqProj, _ := http.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewBuffer(projBody))
		reqProj.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wProj, reqProj)
		if wProj.Code != http.StatusOK {
			t.Fatalf("expected 200 creating project, got %d", wProj.Code)
		}
		var projResp struct {
			Data struct {
				ID uint `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wProj.Body.Bytes(), &projResp)

		// 更新造物进展手记 (story)
		newStory := "今日完成木蜡油第二遍涂装，纹理愈发清晰。"
		updProjBody, _ := json.Marshal(map[string]interface{}{
			"story": newStory,
		})
		wProjUpd := httptest.NewRecorder()
		reqProjUpd, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/projects/%d", projResp.Data.ID), bytes.NewBuffer(updProjBody))
		reqProjUpd.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wProjUpd, reqProjUpd)
		if wProjUpd.Code != http.StatusOK {
			t.Fatalf("expected 200 updating project story, got %d", wProjUpd.Code)
		}

		// 查询项目详情，验证已包含立项与进展两条伴生 LifeEntry
		wProjDetail := httptest.NewRecorder()
		reqProjDetail, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d", projResp.Data.ID), nil)
		r.ServeHTTP(wProjDetail, reqProjDetail)
		var projDetailResp struct {
			Data struct {
				Entries []interface{} `json:"entries"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wProjDetail.Body.Bytes(), &projDetailResp)
		if len(projDetailResp.Data.Entries) < 2 {
			t.Fatalf("expected at least 2 companion LifeEntries for project (creation + story update), got %d", len(projDetailResp.Data.Entries))
		}

		// (c) 2.3 主流自然下渗：在时间流直接创建带 city_code 的 LifeEntry (如景德镇 360200)，自动点亮足迹
		entryJdz := CreateEntryRequest{
			Type:       "life",
			Title:      "三宝村寻瓷",
			Content:    "在景德镇三宝村听溪水与窑火。",
			OccurredAt: "2026-10-11 16:00:00",
			CityCode:   "360200",
			CityName:   "景德镇市",
		}
		entryBody, _ := json.Marshal(entryJdz)
		wEntry := httptest.NewRecorder()
		reqEntry, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(entryBody))
		reqEntry.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wEntry, reqEntry)
		if wEntry.Code != http.StatusOK {
			t.Fatalf("expected 200 creating entry with city_code, got %d", wEntry.Code)
		}

		// 验证景德镇 (360200) 已在城市回忆抽屉与全国足迹图谱中自动点亮
		wJdzDrawer := httptest.NewRecorder()
		reqJdzDrawer, _ := http.NewRequest(http.MethodGet, "/api/v1/footprints/cities/360200/drawer", nil)
		r.ServeHTTP(wJdzDrawer, reqJdzDrawer)
		if wJdzDrawer.Code != http.StatusOK {
			t.Fatalf("expected 200 on auto-infiltrated city drawer, got %d", wJdzDrawer.Code)
		}
		var jdzDrawerResp struct {
			Data struct {
				CityName   string `json:"city_name"`
				VisitStats struct {
					TotalVisits int `json:"total_visits"`
				} `json:"visit_stats"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wJdzDrawer.Body.Bytes(), &jdzDrawerResp)
		if jdzDrawerResp.Data.CityName != "景德镇市" || jdzDrawerResp.Data.VisitStats.TotalVisits < 1 {
			t.Fatalf("expected natural infiltration to light up 景德镇市 (360200), got %+v", jdzDrawerResp.Data)
		}
	})

	t.Run("4. Bi-directional soft delete synchronization between Entry and Footprint", func(t *testing.T) {
		// Direction 1: Soft delete Entry -> Infiltrated Footprint is soft deleted
		entryPayload := map[string]interface{}{
			"type":        "life",
			"title":       "姑苏游记",
			"content":     "游览平江路与拙政园",
			"city_code":   "320500",
			"city_name":   "苏州市",
			"occurred_at": "2026-05-01 10:00:00",
		}
		entryBody, _ := json.Marshal(entryPayload)
		wEntry := httptest.NewRecorder()
		reqEntry, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(entryBody))
		reqEntry.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wEntry, reqEntry)
		if wEntry.Code != http.StatusOK {
			t.Fatalf("failed to create entry: %d", wEntry.Code)
		}
		var createdEntry struct {
			Data struct {
				ID uint `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wEntry.Body.Bytes(), &createdEntry)

		// 检查下渗足迹存在
		var fp苏州 models.Footprint
		if err := db.DB.Where("life_entry_id = ?", createdEntry.Data.ID).First(&fp苏州).Error; err != nil {
			t.Fatalf("expected infiltrated footprint for Suzhou, got error: %v", err)
		}

		// 软删 Entry
		wDelEntry := httptest.NewRecorder()
		reqDelEntry, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/entries/%d", createdEntry.Data.ID), nil)
		r.ServeHTTP(wDelEntry, reqDelEntry)
		if wDelEntry.Code != http.StatusOK {
			t.Fatalf("failed to delete entry: %d", wDelEntry.Code)
		}

		// 验证下渗足迹也被软删
		var checkFP models.Footprint
		if err := db.DB.Where("id = ?", fp苏州.ID).First(&checkFP).Error; err == nil {
			t.Fatalf("expected infiltrated footprint to be soft-deleted, but it was found")
		}
		var unscopedFP models.Footprint
		if err := db.DB.Unscoped().Where("id = ?", fp苏州.ID).First(&unscopedFP).Error; err != nil || unscopedFP.DeletedAt.Valid == false {
			t.Fatalf("expected footprint to exist in unscoped with deleted_at set")
		}

		// Direction 2: Soft delete Footprint -> Companion Entry is soft deleted
		fpPayload := CreateFootprintRequest{
			CityCode:  "420100",
			CityName:  "武汉市",
			VisitedAt: "2026-06-01 12:00:00",
			Note:      "黄鹤楼远眺长江",
		}
		fpBody, _ := json.Marshal(fpPayload)
		wFP := httptest.NewRecorder()
		reqFP, _ := http.NewRequest(http.MethodPost, "/api/v1/footprints", bytes.NewBuffer(fpBody))
		reqFP.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wFP, reqFP)
		if wFP.Code != http.StatusOK {
			t.Fatalf("failed to create footprint: %d", wFP.Code)
		}
		var createdFP struct {
			Data struct {
				ID          uint  `json:"id"`
				LifeEntryID *uint `json:"life_entry_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(wFP.Body.Bytes(), &createdFP)
		if createdFP.Data.LifeEntryID == nil {
			t.Fatalf("expected companion life_entry_id")
		}

		// 软删 Footprint
		wDelFP := httptest.NewRecorder()
		reqDelFP, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/footprints/%d", createdFP.Data.ID), nil)
		r.ServeHTTP(wDelFP, reqDelFP)
		if wDelFP.Code != http.StatusOK {
			t.Fatalf("failed to delete footprint: %d", wDelFP.Code)
		}

		// 验证伴生 Entry 也被软删
		var checkEntry models.LifeEntry
		if err := db.DB.Where("id = ?", *createdFP.Data.LifeEntryID).First(&checkEntry).Error; err == nil {
			t.Fatalf("expected companion entry to be soft-deleted, but it was found")
		}
		var unscopedEntry models.LifeEntry
		if err := db.DB.Unscoped().Where("id = ?", *createdFP.Data.LifeEntryID).First(&unscopedEntry).Error; err != nil || unscopedEntry.DeletedAt.Valid == false {
			t.Fatalf("expected companion entry to exist in unscoped with deleted_at set")
		}
	})
}

