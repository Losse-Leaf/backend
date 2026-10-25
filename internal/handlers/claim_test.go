package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/authclaim"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

func setupClaimRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/system/status", GetSystemStatus)
	r.POST("/api/v1/auth/claim", ClaimDevice)
	return r
}

func postClaim(t *testing.T, r *gin.Engine, code string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"code":        code,
		"device_name": "Safari · macOS",
		"platform":    "web",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/claim", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestClaimDeviceLifecycle(t *testing.T) {
	setupTestFullDB(t)
	db.DB.Create(&models.SiteConfig{Initialized: false})
	claimCodeManager = authclaim.NewManager()
	r := setupClaimRouter()

	code, _, err := StartClaimCode()
	if err != nil {
		t.Fatal(err)
	}

	// 未初始化时状态探针只暴露 initialized / site_name / curator_name。
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	statusW := httptest.NewRecorder()
	r.ServeHTTP(statusW, statusReq)
	if statusW.Code != http.StatusOK {
		t.Fatalf("status code = %d", statusW.Code)
	}
	var statusRes struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(statusW.Body.Bytes(), &statusRes); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(statusRes.Data) != 3 {
		t.Fatalf("system status must expose exactly 3 fields, got %v", statusRes.Data)
	}
	for _, key := range []string{"initialized", "site_name", "curator_name"} {
		if _, ok := statusRes.Data[key]; !ok {
			t.Fatalf("system status missing %q: %v", key, statusRes.Data)
		}
	}

	// 错误认领码：401，且不会创建任何设备。
	for i := 0; i < 4; i++ {
		w := postClaim(t, r, "WRONG-CODE")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d: %s", i+1, w.Code, w.Body.String())
		}
	}
	var deviceCount int64
	db.DB.Model(&models.Device{}).Count(&deviceCount)
	if deviceCount != 0 {
		t.Fatalf("failed claims must not create devices, got %d", deviceCount)
	}

	// 正确认领码忽略大小写与连字符。
	w := postClaim(t, r, strings.ToLower(authclaim.Format(code)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on claim, got %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		Data struct {
			DeviceID    string `json:"device_id"`
			DeviceToken string `json:"device_token"`
			Initialized bool   `json:"initialized"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if !res.Data.Initialized || !strings.HasPrefix(res.Data.DeviceID, "dev_web_") || !strings.HasPrefix(res.Data.DeviceToken, "lldt_") {
		t.Fatalf("unexpected claim response: %+v", res.Data)
	}

	// 下发 HttpOnly 会话 Cookie，且数据库只保存令牌哈希。
	cookies := w.Result().Cookies()
	foundCookie := false
	for _, ck := range cookies {
		if ck.Name == middleware.SessionCookieName && ck.Value == res.Data.DeviceToken && ck.HttpOnly {
			foundCookie = true
		}
	}
	if !foundCookie {
		t.Fatalf("expected HttpOnly session cookie carrying the device token")
	}

	var cfg models.SiteConfig
	db.DB.First(&cfg)
	if !cfg.Initialized {
		t.Fatal("claim success must flip initialized to true")
	}

	var device models.Device
	if err := db.DB.Where("device_id = ?", res.Data.DeviceID).First(&device).Error; err != nil {
		t.Fatalf("device not created: %v", err)
	}
	if device.TokenHash != middleware.HashToken(res.Data.DeviceToken) {
		t.Fatal("device must store only the SHA-256 hash of the token")
	}
	if device.Status != "active" {
		t.Fatalf("device status = %q, want active", device.Status)
	}

	// 认领码已消费，入口永久关闭：即使再生成新码也返回 404。
	if _, _, err := StartClaimCode(); err != nil {
		t.Fatal(err)
	}
	closed := postClaim(t, r, code)
	if closed.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after claim, got %d: %s", closed.Code, closed.Body.String())
	}
}

func TestClaimDeviceFifthFailureRotatesCode(t *testing.T) {
	setupTestFullDB(t)
	db.DB.Create(&models.SiteConfig{Initialized: false})
	claimCodeManager = authclaim.NewManager()
	r := setupClaimRouter()

	oldCode, _, err := StartClaimCode()
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < authclaim.MaxFailures; i++ {
		w := postClaim(t, r, "WRONG-CODE")
		if i < authclaim.MaxFailures-1 && w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i+1, w.Code)
		}
		if i == authclaim.MaxFailures-1 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("5th failure: expected 429, got %d: %s", w.Code, w.Body.String())
		}
	}

	// 旧码已作废，系统仍未初始化。
	if w := postClaim(t, r, oldCode); w.Code != http.StatusUnauthorized {
		t.Fatalf("old code must be invalid after rotation, got %d", w.Code)
	}
	var cfg models.SiteConfig
	db.DB.First(&cfg)
	if cfg.Initialized {
		t.Fatal("failed claims must not initialize the system")
	}

	// 新码（此处直接重新生成模拟日志中的新码）仍可正常认领。
	newCode, _, err := StartClaimCode()
	if err != nil {
		t.Fatal(err)
	}
	if w := postClaim(t, r, newCode); w.Code != http.StatusOK {
		t.Fatalf("expected 200 with fresh code, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClaimDeviceExpiryRequiresRestart(t *testing.T) {
	setupTestFullDB(t)
	db.DB.Create(&models.SiteConfig{Initialized: false})
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	claimCodeManager = authclaim.NewManager(authclaim.WithClock(func() time.Time { return now }))
	r := setupClaimRouter()

	code, _, err := StartClaimCode()
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(authclaim.TTL + time.Second)
	w := postClaim(t, r, code)
	if w.Code != http.StatusGone {
		t.Fatalf("expected 410 for expired code, got %d: %s", w.Code, w.Body.String())
	}

	// 过期后同一进程内不会自动续期；重启（新建 Manager）后才会有新码。
	if w := postClaim(t, r, code); w.Code != http.StatusGone {
		t.Fatalf("expected 410 after code cleared, got %d", w.Code)
	}
	claimCodeManager = authclaim.NewManager(authclaim.WithClock(func() time.Time { return now }))
	fresh, _, err := StartClaimCode()
	if err != nil {
		t.Fatal(err)
	}
	if w := postClaim(t, r, fresh); w.Code != http.StatusOK {
		t.Fatalf("expected 200 after restart, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClaimEntryClosedWhenAlreadyInitialized(t *testing.T) {
	setupTestFullDB(t)
	db.DB.Create(&models.SiteConfig{Initialized: true})
	claimCodeManager = authclaim.NewManager()
	r := setupClaimRouter()

	code, _, err := StartClaimCode()
	if err != nil {
		t.Fatal(err)
	}
	if w := postClaim(t, r, code); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when already initialized, got %d: %s", w.Code, w.Body.String())
	}

	var deviceCount int64
	db.DB.Model(&models.Device{}).Count(&deviceCount)
	if deviceCount != 0 {
		t.Fatalf("closed claim entry must not create devices, got %d", deviceCount)
	}
}
