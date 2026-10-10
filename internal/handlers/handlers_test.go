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

func TestIncrementalSyncAndETagCaching(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.POST("/api/v1/entries", CreateLifeEntry)
	r.GET("/api/v1/entries", GetLifeEntriesV1)
	r.DELETE("/api/v1/entries/:id", DeleteLifeEntry)

	// 1. Create initial entry
	req1 := CreateEntryRequest{
		Type:       "thought",
		Title:      "First Entry",
		Content:    "Content 1",
		OccurredAt: "2026-10-10T08:00:00Z",
	}
	bodyBytes, _ := json.Marshal(req1)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to create entry 1")
	}

	// 2. Query all entries and check ETag
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("expected ETag header to be present")
	}

	// 3. Conditional request with matching If-None-Match returns 304 Not Modified
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries", nil)
	req.Header.Set("If-None-Match", etag)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", w.Code)
	}

	// 4. Create second entry
	req2 := CreateEntryRequest{
		Type:       "thought",
		Title:      "Second Entry",
		Content:    "Content 2",
		OccurredAt: "2026-10-10T09:00:00Z",
	}
	bodyBytes, _ = json.Marshal(req2)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// 5. Test soft delete on entry 2
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/v1/entries/2", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on soft delete, got %d", w.Code)
	}

	// 6. Normal query excludes soft-deleted entry 2
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries", nil)
	r.ServeHTTP(w, req)
	var listResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	listData := listResp["data"].(map[string]interface{})
	if int(listData["total"].(float64)) != 1 {
		t.Fatalf("expected 1 active entry after soft delete, got %v", listData["total"])
	}

	// 7. Incremental sync with ?since= returns modified/deleted entries with deleted_at set
	sinceQuery := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?since="+sinceQuery, nil)
	r.ServeHTTP(w, req)
	var syncResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &syncResp)
	syncData := syncResp["data"].(map[string]interface{})
	entries := syncData["entries"].([]interface{})
	var foundDeleted bool
	for _, raw := range entries {
		item := raw.(map[string]interface{})
		if int(item["id"].(float64)) == 2 {
			if val, ok := item["deleted_at"]; ok && val != nil {
				foundDeleted = true
			}
		}
	}
	if !foundDeleted {
		t.Fatalf("expected soft-deleted entry to have deleted_at in sync response: %s", w.Body.String())
	}
}

func TestTransactionAndProjectIncrementalSyncAndETag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.POST("/api/v1/transactions", CreateTransaction)
	r.GET("/api/v1/transactions", GetTransactions)
	r.DELETE("/api/v1/transactions/:id", DeleteTransaction)

	r.POST("/api/v1/projects", CreateProject)
	r.GET("/api/v1/projects", GetProjectsV1)
	r.DELETE("/api/v1/projects/:id", DeleteProject)

	// 1. Transactions CRUD, ETag & Soft Delete
	txReq := TransactionRequest{
		Amount:        2500,
		Type:          "expense",
		Category:      "餐饮",
		Title:         "手冲咖啡",
		OccurredAt:    "2026-10-10 10:00:00",
		PaymentMethod: "wechat",
	}
	body, _ := json.Marshal(txReq)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to create transaction")
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/transactions", nil)
	r.ServeHTTP(w, req)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("expected ETag for transactions")
	}

	// 304 on matching ETag
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/transactions", nil)
	req.Header.Set("If-None-Match", etag)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 for transactions, got %d", w.Code)
	}

	// Soft delete
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/v1/transactions/1", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to soft delete transaction")
	}

	// Normal list returns empty
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/transactions", nil)
	r.ServeHTTP(w, req)
	var txListResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &txListResp)
	txs := txListResp["data"].([]interface{})
	if len(txs) != 0 {
		t.Fatalf("expected 0 active transactions, got %d", len(txs))
	}

	// Sync with since returns soft deleted transaction
	w = httptest.NewRecorder()
	sinceTime := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/transactions?since="+sinceTime, nil)
	r.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &txListResp)
	txs = txListResp["data"].([]interface{})
	if len(txs) != 1 {
		t.Fatalf("expected 1 transaction in sync, got %d", len(txs))
	}
	item := txs[0].(map[string]interface{})
	if item["deleted_at"] == nil {
		t.Fatalf("expected deleted_at to be populated in transaction")
	}

	// 2. Projects CRUD, ETag & Soft Delete
	projReq := ProjectRequest{
		Title:       "Test Project",
		Description: "A great project",
		Category:    "code",
	}
	body, _ = json.Marshal(projReq)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to create project")
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	r.ServeHTTP(w, req)
	projEtag := w.Header().Get("ETag")
	if projEtag == "" {
		t.Fatalf("expected ETag for projects")
	}

	// 304 on matching ETag
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("If-None-Match", projEtag)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 for projects, got %d", w.Code)
	}

	// Soft delete project
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/v1/projects/1", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to soft delete project")
	}

	// Normal list returns empty
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	r.ServeHTTP(w, req)
	var projListResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &projListResp)
	projs := projListResp["data"].([]interface{})
	if len(projs) != 0 {
		t.Fatalf("expected 0 active projects, got %d", len(projs))
	}
}
