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

// 聚合 SQL 必须剥离 LIMIT/OFFSET，否则 total 会退化成「本页条数」，
// ETag 也会漏掉后续页的变更。
func TestListAggregateIgnoresPagination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	enableAllPublicModulesForTest(t)

	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		seedEntryForCache(t, fmt.Sprintf("entry-%d", i), base.Add(time.Duration(i)*time.Hour))
	}

	r := gin.New()
	r.GET("/api/v1/entries", GetLifeEntriesV1)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/entries?page=1&page_size=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Total   int                `json:"total"`
			Entries []models.LifeEntry `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Entries) != 1 {
		t.Fatalf("expected 1 entry on page, got %d", len(resp.Data.Entries))
	}
	if resp.Data.Total != 3 {
		t.Fatalf("aggregate must ignore LIMIT/OFFSET: total=%d, want 3", resp.Data.Total)
	}
}

// 散页内嵌的心愿必须与心愿列表同口径脱敏，访客不能绕过 /wishes 看到实付金额。
func TestEmbeddedWishFinancialRedaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	enableAllPublicModulesForTest(t)

	adminKey := "lldt_audit_embed_wish"
	seedTestAdminDevice(adminKey)

	txID := uint(7)
	wish := models.Wish{
		Title:         "一台胶片机",
		Kind:          "item",
		Budget:        500000,
		ActualCost:    480000,
		Status:        "fulfilled",
		TransactionID: &txID,
	}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatalf("seed wish: %v", err)
	}

	entry := models.LifeEntry{
		Type:       "wish",
		Title:      "胶片机到货",
		Content:    "终于到了",
		OccurredAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local),
		WishID:     &wish.ID,
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	r := gin.New()
	r.GET("/api/v1/entries", GetLifeEntriesV1)
	r.GET("/api/v1/entries/:id", GetLifeEntryByID)

	assertRedacted := func(body []byte, label string) {
		t.Helper()
		var resp struct {
			Data struct {
				Entries []models.LifeEntry `json:"entries"`
				Wish    *models.Wish       `json:"wish"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("%s decode: %v", label, err)
		}
		check := func(w *models.Wish) {
			if w == nil {
				t.Fatalf("%s: embedded wish missing", label)
			}
			if w.ActualCost != 0 || w.TransactionID != nil {
				t.Fatalf("%s leaks wish finances: actual_cost=%d tx=%v",
					label, w.ActualCost, w.TransactionID)
			}
			if w.Budget != 500000 {
				t.Fatalf("%s: budget is public and must stay, got %d", label, w.Budget)
			}
		}
		if len(resp.Data.Entries) > 0 {
			check(resp.Data.Entries[0].Wish)
			return
		}
		check(resp.Data.Wish)
	}

	guestList := httptest.NewRecorder()
	r.ServeHTTP(guestList, httptest.NewRequest(http.MethodGet, "/api/v1/entries", nil))
	assertRedacted(guestList.Body.Bytes(), "guest list")

	guestDetail := httptest.NewRecorder()
	r.ServeHTTP(guestDetail, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/entries/%d", entry.ID), nil))
	assertRedacted(guestDetail.Body.Bytes(), "guest detail")

	adminDetail := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/entries/%d", entry.ID), nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	r.ServeHTTP(adminDetail, req)
	if adminDetail.Code != http.StatusOK {
		t.Fatalf("admin detail failed: %d %s", adminDetail.Code, adminDetail.Body.String())
	}
	var adminResp struct {
		Data models.LifeEntry `json:"data"`
	}
	if err := json.Unmarshal(adminDetail.Body.Bytes(), &adminResp); err != nil {
		t.Fatalf("admin decode: %v", err)
	}
	if adminResp.Data.Wish == nil || adminResp.Data.Wish.ActualCost != 480000 {
		t.Fatalf("admin must see full wish finances, got %+v", adminResp.Data.Wish)
	}
}

// 增量游标格式错误必须 400；静默忽略会退化成「全量列表」，客户端误判为增量结果。
func TestInvalidSinceIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	adminKey := "lldt_invalid_since"
	seedTestAdminDevice(adminKey)

	r := gin.New()
	r.GET("/api/v1/entries", GetLifeEntriesV1)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/entries?since=not-a-time", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid since must be 400, got %d: %s", w.Code, w.Body.String())
	}
}

// 无偏移时间串必须按北京时间解释（RC-134），不能跟随服务器本地时区。
func TestAggregateTimeNaiveUsesBeijing(t *testing.T) {
	got, err := parseAggregateTime("2026-10-23 12:00:00")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, offset := got.Zone()
	if offset != 8*3600 {
		t.Fatalf("naive timestamp must be Beijing +08:00, got offset=%d", offset)
	}
}

// 清理孤立上传只能删「entry/wish/footprint 全为空」的真孤儿；
// 心愿/足迹仍在引用的共享照片绝不能被误删。
func TestCleanupUploadKeepsSharedAttachments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	wishID := uint(8)
	shared := models.Attachment{ObjectKey: "shared-wish.jpg", WishID: &wishID}
	orphan := models.Attachment{ObjectKey: "orphan.jpg"}
	if err := db.DB.Create(&shared).Error; err != nil {
		t.Fatalf("seed shared: %v", err)
	}
	if err := db.DB.Create(&orphan).Error; err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	r := gin.New()
	r.POST("/api/v1/uploads/cleanup", CleanupUpload)
	body, _ := json.Marshal(CleanupUploadRequest{ObjectKeys: []string{"shared-wish.jpg", "orphan.jpg"}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/cleanup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cleanup failed: %d %s", w.Code, w.Body.String())
	}

	var sharedCount, orphanCount int64
	db.DB.Model(&models.Attachment{}).Where("object_key = ?", "shared-wish.jpg").Count(&sharedCount)
	db.DB.Model(&models.Attachment{}).Where("object_key = ?", "orphan.jpg").Count(&orphanCount)
	if sharedCount != 1 {
		t.Fatalf("shared attachment must be kept")
	}
	if orphanCount != 0 {
		t.Fatalf("orphan attachment should be removed")
	}
}

// 显式清空契约：content 为空串、location 为空串必须真正清空。
func TestEntryExplicitClearContentAndLocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	adminKey := "lldt_clear_entry"
	seedTestAdminDevice(adminKey)

	entry := models.LifeEntry{
		Type: "life", Title: "有正文", Content: "要被清空",
		OccurredAt: time.Now(), Location: "西湖边",
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := gin.New()
	r.PUT("/api/v1/entries/:id", UpdateLifeEntry)
	body := `{"content":""}`
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/entries/%d", entry.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", w.Code, w.Body.String())
	}

	var got models.LifeEntry
	db.DB.First(&got, entry.ID)
	if got.Content != "" {
		t.Fatalf("explicit clear failed: content=%q", got.Content)
	}
	if got.Location != "西湖边" {
		t.Fatalf("未携带 location 时应保持原值, 实际 %q", got.Location)
	}

	// 全清路径：显式携带 location 空串才清空地点（契约 C）
	body2 := `{"content":"","location":""}`
	req2 := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/entries/%d", entry.ID), bytes.NewBufferString(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+adminKey)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", w2.Code, w2.Body.String())
	}
	db.DB.First(&got, entry.ID)
	if got.Location != "" {
		t.Fatalf("显式携带 location 空串应清空地点, 实际 %q", got.Location)
	}
}
