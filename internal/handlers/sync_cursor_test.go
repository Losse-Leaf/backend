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

// MOB-P1-36：服务端权威增量游标 + 含等号边界的回归测试
//
// 这组用例守住两件事：
//  1. 游标边界**含等号** —— 恰好等于游标那一秒发生的修改不会被下一轮漏掉；
//  2. `X-Sync-Cursor` 响应头可被客户端原样回传为 ?since=，往返不丢数据。

// syncCursorOfEntries 从响应体里取出全部条目的 id
func entryIDsFromList(t *testing.T, body []byte) []int {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	data := resp["data"].(map[string]interface{})
	rawEntries, ok := data["entries"].([]interface{})
	if !ok {
		t.Fatalf("expected entries array, got %T", data["entries"])
	}
	ids := make([]int, 0, len(rawEntries))
	for _, raw := range rawEntries {
		ids = append(ids, int(raw.(map[string]interface{})["id"].(float64)))
	}
	return ids
}

func containsID(ids []int, target int) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func TestSyncCursorHeaderIsAuthoritativeAndInclusive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	adminKey := "lldt_cursor_authoritative"
	seedTestAdminDevice(adminKey)

	r := gin.New()
	r.POST("/api/v1/entries", CreateLifeEntry)
	r.GET("/api/v1/entries", GetLifeEntriesV1)

	// 建一条记录
	body, _ := json.Marshal(CreateEntryRequest{
		Type:       "life",
		Title:      "游标记录",
		Content:    "内容",
		OccurredAt: "2026-10-10T08:00:00Z",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create entry failed: %d", w.Code)
	}

	// 全量拉取：响应头必须给出服务端权威游标
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries", nil)
	r.ServeHTTP(w, req)
	cursor := w.Header().Get(SyncCursorHeader)
	if cursor == "" {
		t.Fatalf("expected %s header to be present", SyncCursorHeader)
	}
	if _, err := time.Parse(time.RFC3339, cursor); err != nil {
		t.Fatalf("cursor %q is not RFC3339: %v", cursor, err)
	}

	// 游标必须能被服务端自己解析（即客户端可以原样回传）
	if _, err := ParseSyncSinceTime(cursor); err != nil {
		t.Fatalf("cursor %q must be parsable by ParseSyncSinceTime: %v", cursor, err)
	}

	// 用游标原样回传：记录必须**仍然被带回**（含等号边界 ⇒ 不漏拉）
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?since="+cursor, nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("incremental fetch failed: %d", w.Code)
	}
	ids := entryIDsFromList(t, w.Body.Bytes())
	if !containsID(ids, 1) {
		t.Fatalf("record updated exactly at the cursor must be returned again (inclusive boundary), got ids %v", ids)
	}
}

func TestSyncCursorCoversSoftDeletedRecords(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	adminKey := "lldt_cursor_tombstone"
	seedTestAdminDevice(adminKey)

	r := gin.New()
	r.POST("/api/v1/entries", CreateLifeEntry)
	r.GET("/api/v1/entries", GetLifeEntriesV1)
	r.DELETE("/api/v1/entries/:id", DeleteLifeEntry)

	body, _ := json.Marshal(CreateEntryRequest{
		Type:       "life",
		Title:      "将被软删除",
		Content:    "内容",
		OccurredAt: "2026-10-10T08:00:00Z",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// 软删除
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodDelete, "/api/v1/entries/1", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("soft delete failed: %d", w.Code)
	}

	// 增量拉取：墓碑必须带 deleted_at 回来，游标也必须覆盖到删除时刻
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/entries?since=2000-01-01T00:00:00%2B08:00", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	r.ServeHTTP(w, req)
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	entries := data["entries"].([]interface{})
	if len(entries) == 0 {
		t.Fatalf("expected the tombstone to be returned in incremental sync")
	}
	item := entries[0].(map[string]interface{})
	if item["deleted_at"] == nil {
		t.Fatalf("expected deleted_at on tombstone, got %v", item)
	}
	if w.Header().Get(SyncCursorHeader) == "" {
		t.Fatalf("expected %s header on incremental response", SyncCursorHeader)
	}
}

// MOB-P1-69：Cookie 承载的令牌必须能通过鉴权中间件
func TestSessionCookieAuthenticates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	rawToken := "lldt_cookie_test_token"
	db.DB.Create(&models.Device{
		DeviceID:  "dev_cookie_test",
		Name:      "Cookie 测试设备",
		Platform:  "web",
		TokenHash: middleware.HashToken(rawToken),
		Status:    "active",
	})

	r := gin.New()
	r.GET("/api/v1/auth/session", GetSessionStatus)

	// 1. 不带任何凭据 ⇒ 访客
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	r.ServeHTTP(w, req)
	assertSessionRole(t, w.Body.Bytes(), false)

	// 2. 只带 HttpOnly Cookie ⇒ 主人
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: rawToken})
	r.ServeHTTP(w, req)
	assertSessionRole(t, w.Body.Bytes(), true)
}

// MOB-P1-69：登出必须清掉 Cookie
func TestLogoutClearsSessionCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.POST("/api/v1/auth/logout", LogoutSession)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var cleared bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == middleware.SessionCookieName && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("expected session cookie to be cleared on logout")
	}
}

func assertSessionRole(t *testing.T, body []byte, wantAdmin bool) {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal session response: %v", err)
	}
	data := resp["data"].(map[string]interface{})
	got := data["is_admin"].(bool)
	if got != wantAdmin {
		t.Fatalf("expected is_admin=%v, got %v", wantAdmin, got)
	}
}
