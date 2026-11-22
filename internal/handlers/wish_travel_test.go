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

func TestWishAndAlmanacModules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	enableAllPublicModulesForTest(t)

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		v1.GET("/wishes", GetWishes)
		v1.GET("/wishes/:id", GetWishByID)
		v1.POST("/wishes", CreateWish)
		v1.PUT("/wishes/:id", UpdateWish)
		v1.POST("/wishes/:id/resolve", ResolveWish)
		v1.DELETE("/wishes/:id", DeleteWish)

		v1.GET("/almanac/summary", GetAnnualAlmanacSummary)
	}

	// 1. Wish Creation and Resolve with Transaction + LifeEntry Linkage
	t.Run("Wish Creation and Resolve Ritual Linkage", func(t *testing.T) {
		wishReq := CreateWishRequest{
			Title:      "云南大理旅居两周",
			Kind:       "travel",
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
		if resolvedWish["transaction_id"] == nil || resolvedWish["life_entry_id"] == nil {
			t.Fatalf("expected transaction_id and life_entry_id linkages to be non-nil: %+v", resolvedWish)
		}
	})

	// 2. Annual Almanac Summary
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
