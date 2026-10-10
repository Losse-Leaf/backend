package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestParseFlexTime(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected time.Time
		hasError bool
	}{
		{
			name:     "HTML5 datetime-local without seconds",
			input:    "2026-10-09T14:30",
			expected: time.Date(2026, 10, 9, 14, 30, 0, 0, time.Local),
			hasError: false,
		},
		{
			name:     "HTML5 datetime-local with seconds",
			input:    "2026-10-09T14:30:45",
			expected: time.Date(2026, 10, 9, 14, 30, 45, 0, time.Local),
			hasError: false,
		},
		{
			name:     "Standard datetime with space",
			input:    "2026-10-09 14:30",
			expected: time.Date(2026, 10, 9, 14, 30, 0, 0, time.Local),
			hasError: false,
		},
		{
			name:     "Standard date only",
			input:    "2026-10-09",
			expected: time.Date(2026, 10, 9, 0, 0, 0, 0, time.Local),
			hasError: false,
		},
		{
			name:     "Dot separated date",
			input:    "2026.10.09",
			expected: time.Date(2026, 10, 9, 0, 0, 0, 0, time.Local),
			hasError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFlexTime(tc.input)
			if tc.hasError && err == nil {
				t.Fatalf("expected error for %s, got nil", tc.input)
			}
			if !tc.hasError && err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.input, err)
			}
			if got.Year() != tc.expected.Year() ||
				got.Month() != tc.expected.Month() ||
				got.Day() != tc.expected.Day() ||
				got.Hour() != tc.expected.Hour() ||
				got.Minute() != tc.expected.Minute() {
				t.Errorf("input %s: got %v, expected %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestLifeEntryCRUDAndFiltering(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.POST("/api/v1/entries", CreateLifeEntry)
	r.GET("/api/v1/entries", GetLifeEntriesV1)
	r.GET("/api/v1/entries/:id", GetLifeEntryByID)
	r.PUT("/api/v1/entries/:id", UpdateLifeEntry)
	r.DELETE("/api/v1/entries/:id", DeleteLifeEntry)
	r.GET("/api/v1/stats", GetGlobalStats)

	// 1. Create entry for 2026
	req2026 := CreateEntryRequest{
		Type:       "thought",
		Title:      "2026 Thought",
		Content:    "Thinking in 2026",
		OccurredAt: "2026-05-20T10:00:00Z",
	}
	bodyBytes, _ := json.Marshal(req2026)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on create, got %d: %s", w.Code, w.Body.String())
	}

	var createResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	entryData := createResp["data"].(map[string]interface{})
	if _, ok := entryData["date"]; ok {
		t.Errorf("date field should not be present in LifeEntry json response")
	}
	if _, ok := entryData["year"]; ok {
		t.Errorf("year field should not be present in LifeEntry json response")
	}
	if _, ok := entryData["images"]; ok {
		t.Errorf("images field should not be present in LifeEntry json response")
	}

	// 2. Create entry for 2025
	req2025 := CreateEntryRequest{
		Type:       "photo",
		Title:      "2025 Photo",
		Content:    "Photo taken in 2025",
		OccurredAt: "2025-11-11T12:00:00Z",
	}
	bodyBytes, _ = json.Marshal(req2025)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on create 2025 entry, got %d: %s", w.Code, w.Body.String())
	}

	// 3. Query with year=2026
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?year=2026", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on query year=2026, got %d", w.Code)
	}
	var query2026Resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &query2026Resp)
	data2026 := query2026Resp["data"].(map[string]interface{})
	if int(data2026["total"].(float64)) != 1 {
		t.Errorf("expected 1 entry for 2026, got %v", data2026["total"])
	}

	// 4. Query with year=2025
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?year=2025", nil)
	r.ServeHTTP(w, req)
	var query2025Resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &query2025Resp)
	data2025 := query2025Resp["data"].(map[string]interface{})
	if int(data2025["total"].(float64)) != 1 {
		t.Errorf("expected 1 entry for 2025, got %v", data2025["total"])
	}

	// 5. Query with category=photo
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?category=photo", nil)
	r.ServeHTTP(w, req)
	var queryPhotoResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &queryPhotoResp)
	dataPhoto := queryPhotoResp["data"].(map[string]interface{})
	if int(dataPhoto["total"].(float64)) != 1 {
		t.Errorf("expected 1 photo entry, got %v", dataPhoto["total"])
	}

	// 6. Check stats
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	r.ServeHTTP(w, req)
	var statsResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &statsResp)
	statsData := statsResp["data"].(map[string]interface{})
	if int(statsData["total_entries"].(float64)) != 2 {
		t.Errorf("expected total_entries=2, got %v", statsData["total_entries"])
	}
	if int(statsData["total_photos"].(float64)) != 1 {
		t.Errorf("expected total_photos=1, got %v", statsData["total_photos"])
	}
}
