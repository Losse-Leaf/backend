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
	"loose-leaf/internal/models"
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
		{
			name:     "RFC3339 with +08:00 offset",
			input:    "2026-10-09T14:30:00+08:00",
			expected: time.Date(2026, 10, 9, 14, 30, 0, 0, time.Local),
			hasError: false,
		},
		{
			name:     "RFC3339Nano with +08:00 offset and fractional seconds",
			input:    "2026-10-09T14:30:00.123+08:00",
			expected: time.Date(2026, 10, 9, 14, 30, 0, 0, time.Local),
			hasError: false,
		},
		{
			name:     "ISO8601 with UTC Z and milliseconds (toISOString)",
			input:    "2026-10-09T06:30:00.000Z",
			expected: time.Date(2026, 10, 9, 14, 30, 0, 0, time.Local),
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
		Type:       "life",
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
		Type:       "life",
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

	// 5. Query with category=daily (RC-128 四母体收敛后 daily 视角 = type=life；旧 photo 类型兼容分支已移除)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?category=daily", nil)
	r.ServeHTTP(w, req)
	var queryPhotoResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &queryPhotoResp)
	dataPhoto := queryPhotoResp["data"].(map[string]interface{})
	if int(dataPhoto["total"].(float64)) != 2 {
		t.Errorf("expected 2 life entries, got %v", dataPhoto["total"])
	}

	// 5b. 已移除的 photo 类型视角不再作为兼容分支存在 (BE-36)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?category=photo", nil)
	r.ServeHTTP(w, req)
	var queryLegacyResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &queryLegacyResp)
	dataLegacy := queryLegacyResp["data"].(map[string]interface{})
	if int(dataLegacy["total"].(float64)) != 2 {
		t.Errorf("expected legacy category filter to be a no-op (all 2 entries), got %v", dataLegacy["total"])
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
	// total_photos 口径为 image/* 附件数 (BE-29)，先补一张图片附件与一张非图片附件
	entryID := uint(0)
	var entryRow models.LifeEntry
	if err := db.DB.Order("id ASC").First(&entryRow).Error; err == nil {
		entryID = entryRow.ID
	}
	if entryID > 0 {
		db.DB.Create(&models.Attachment{EntryID: &entryID, ObjectKey: "entries/test/photo.png", OriginalFilename: "photo.png", MimeType: "image/png", Size: 1024})
		db.DB.Create(&models.Attachment{EntryID: &entryID, ObjectKey: "entries/test/note.pdf", OriginalFilename: "note.pdf", MimeType: "application/pdf", Size: 2048})
	}

	InvalidateStatsCache() // 统计接口有 30 秒内存缓存，二次断言前主动失效
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	r.ServeHTTP(w, req)
	var statsResp2 map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &statsResp2)
	statsData2 := statsResp2["data"].(map[string]interface{})
	if int(statsData2["total_photos"].(float64)) != 1 {
		t.Errorf("expected total_photos=1 (image/* only), got %v", statsData2["total_photos"])
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

func TestCursorPaginationOnThisDayAndGlobalSearch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.POST("/api/v1/entries", CreateLifeEntry)
	r.GET("/api/v1/entries", GetLifeEntriesV1)
	r.GET("/api/v1/entries/on-this-day", GetOnThisDayEntries)
	r.GET("/api/v1/search", GlobalSearch)
	r.POST("/api/v1/wishes", CreateWish)
	r.POST("/api/v1/projects", CreateProject)

	// 1. Seed entries across years for On-This-Day & Cursor Pagination
	entriesToSeed := []CreateEntryRequest{
		{
			Type:       "life",
			Title:      "2024 西湖秋日漫步",
			Content:    "在桂花香气中走过苏堤",
			OccurredAt: "2024-10-15T14:00:00+08:00",
			Location:   "杭州西湖",
			CityCode:   "330100",
			CityName:   "杭州市",
		},
		{
			Type:       "life",
			Title:      "2025 景德镇陶艺手记",
			Content:    "拉坯与利坯的静心时刻",
			OccurredAt: "2025-10-15T16:00:00+08:00",
			Location:   "景德镇",
			CityCode:   "360200",
			CityName:   "景德镇市",
		},
		{
			Type:       "life",
			Title:      "2025 深夜阅读",
			Content:    "读完一本纸质散文集",
			OccurredAt: "2025-11-20T22:00:00+08:00",
		},
	}

	for _, item := range entriesToSeed {
		body, _ := json.Marshal(item)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("failed to seed entry %s: %d %s", item.Title, w.Code, w.Body.String())
		}
	}

	// 2. Test Cursor Pagination (before_occurred_at & before_id)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/entries?before_occurred_at=2025-11-01T00:00:00%2B08:00", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on cursor pagination, got %d", w.Code)
	}
	var cursorResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &cursorResp)
	cursorData := cursorResp["data"].(map[string]interface{})
	cursorEntries := cursorData["entries"].([]interface{})
	if len(cursorEntries) != 2 {
		t.Fatalf("expected 2 entries before 2025-11-01, got %d", len(cursorEntries))
	}

	// Test cursor with same timestamp and before_id
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?before_occurred_at=2025-10-15T16:00:00%2B08:00&before_id=2", nil)
	r.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &cursorResp)
	cursorData = cursorResp["data"].(map[string]interface{})
	cursorEntries = cursorData["entries"].([]interface{})
	if len(cursorEntries) != 1 {
		t.Fatalf("expected 1 entry before (2025-10-15T16:00:00+08:00, id=2), got %d", len(cursorEntries))
	}

	// 3. Test On-This-Day (GET /api/v1/entries/on-this-day?month=10&day=15)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries/on-this-day?month=10&day=15", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on on-this-day, got %d", w.Code)
	}
	var otdResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &otdResp)
	otdData := otdResp["data"].(map[string]interface{})
	otdEntries := otdData["entries"].([]interface{})
	if len(otdEntries) != 2 {
		t.Fatalf("expected 2 historical entries on Oct 15, got %d", len(otdEntries))
	}

	// 4. Seed Wish and Project and test GlobalSearch
	wishBody, _ := json.Marshal(CreateWishRequest{
		Title:    "收藏一把西湖手工绸伞",
		Kind:     "item",
		Note:     "杭州非遗手艺",
		CityName: "杭州市",
		Budget:   38000,
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/wishes", bytes.NewBuffer(wishBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	projBody, _ := json.Marshal(ProjectRequest{
		Title:       "西湖植物志图鉴",
		Description: "整理杭州西湖四季植物标本",
		Category:    "人文自然",
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewBuffer(projBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/search?q=西湖&limit=20", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on global search, got %d", w.Code)
	}
	var searchResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &searchResp)
	searchData := searchResp["data"].(map[string]interface{})
	if len(searchData["entries"].([]interface{})) == 0 {
		t.Errorf("expected entries matching '西湖'")
	}
	if len(searchData["wishes"].([]interface{})) == 0 {
		t.Errorf("expected wishes matching '西湖'")
	}
	if len(searchData["projects"].([]interface{})) == 0 {
		t.Errorf("expected projects matching '西湖'")
	}
}

// MOB-P1-xx：编辑账目时若请求体**没带 type**，服务端必须**保留原值**。
//
// 踩过的坑：客户端开启了 encodeDefaults=false，等于默认值的字段会被整个省略 ⇒
// PUT 请求体里没有 type 键 ⇒ req.Type 绑定成 ""。
// 原先直接 `tx.Type = req.Type` ⇒ 支出账目编辑一次就变成空串，
// 不再命中任何 type='expense' 的过滤 ⇒ 从支出统计与日合计里消失；
// 而本地还是乐观写回的 "expense" ⇒ 两端分叉，下一轮拉取才把空串同步回去。
//
// Update 的语义本就是「部分更新」：空 = 这次没带这个字段 ⇒ 保留原值。
func TestUpdateTransactionKeepsTypeWhenOmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.POST("/api/v1/transactions", CreateTransaction)
	r.PUT("/api/v1/transactions/:id", UpdateTransaction)

	// 建一笔 expense
	body, _ := json.Marshal(TransactionRequest{
		Amount:        2500,
		Type:          "expense",
		Category:      "餐饮",
		Title:         "手冲咖啡",
		OccurredAt:    "2026-10-10 10:00:00",
		PaymentMethod: "wechat",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to create transaction: %d %s", w.Code, w.Body.String())
	}

	// 编辑：金额改了，但**不带 type**（模拟 encodeDefaults=false 省略了默认值字段）
	updateBody := []byte(`{"amount":3000,"category":"餐饮","title":"手冲咖啡（改）","occurred_at":"2026-10-10 10:00:00"}`)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPut, "/api/v1/transactions/1", bytes.NewBuffer(updateBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to update transaction: %d %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data models.Transaction `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v (%s)", err, w.Body.String())
	}
	if resp.Data.Type != "expense" {
		t.Fatalf("省略 type 时应保留原值 expense，实际得到 %q", resp.Data.Type)
	}
	if resp.Data.Amount != 3000 {
		t.Fatalf("金额应当被更新为 3000，实际 %d", resp.Data.Amount)
	}
}
