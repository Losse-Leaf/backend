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
}
