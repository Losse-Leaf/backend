package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupTestFullDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test in-memory db: %v", err)
	}
	_ = database.AutoMigrate(
		&models.SiteConfig{},
		&models.CuratorProfile{},
		&models.Device{},
		&models.PairTicket{},
		&models.Attachment{},
		&models.LifeEntry{},
		&models.Transaction{},
		&models.Project{},
		&models.Wish{},
		&models.Footprint{},
		&models.CreditChannel{},
		&models.CategoryOption{},
	)
	database.Create(&models.CreditChannel{
		Code:         "huabei",
		Name:         "花呗",
		RepaymentDay: 10,
		IsBuiltin:    true,
		Color:        "var(--accent-warm)",
	})
	database.Create(&models.CreditChannel{
		Code:         "baitiao",
		Name:         "京东白条",
		RepaymentDay: 20,
		IsBuiltin:    true,
		Color:        "var(--accent-vermilion)",
	})
	db.DB = database
}

// seedTestAdminDevice 在测试数据库中注册一枚处于 active 状态的受信任设备子钥匙 (BE-18)
func seedTestAdminDevice(rawToken string) {
	now := time.Now()
	db.DB.Create(&models.Device{
		DeviceID:     "dev_web_test_" + generateRandomHex(2),
		Name:         "测试管理员设备",
		Platform:     "web",
		TokenHash:    middleware.HashToken(rawToken),
		Status:       "active",
		PairedAt:     now,
		LastActiveAt: now,
	})
}

func TestProfileAndConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.GET("/api/v1/config", GetConfigV1)
	r.PUT("/api/v1/config", UpdateConfig)
	r.GET("/api/v1/profile", GetCuratorProfile)
	r.PUT("/api/v1/profile", UpdateCuratorProfile)

	// 1. 初始化并获取 Config
	t.Run("Get and update config", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		// 更新 Config
		updatePayload := models.SiteConfig{
			SiteName:    "新散页 · Living Archive",
			CuratorName: "测试主理人",
		}
		body, _ := json.Marshal(updatePayload)
		putReq := httptest.NewRequest(http.MethodPut, "/api/v1/config", bytes.NewReader(body))
		putReq.Header.Set("Content-Type", "application/json")
		putW := httptest.NewRecorder()
		r.ServeHTTP(putW, putReq)

		if putW.Code != http.StatusOK {
			t.Fatalf("expected 200 for update config, got %d", putW.Code)
		}

		var res struct {
			Data models.SiteConfig `json:"data"`
		}
		_ = json.Unmarshal(putW.Body.Bytes(), &res)
		if res.Data.SiteName != "新散页 · Living Archive" || res.Data.CuratorName != "测试主理人" {
			t.Fatalf("unexpected updated config: %+v", res.Data)
		}
	})

	// 2. 初始化并获取 Profile (含 RG-09 email / github 保存与平铺校验)
	t.Run("Get and update curator profile", func(t *testing.T) {
		initialProfile := models.CuratorProfile{
			Name:        "测试主理人",
			Title:       "数字手艺人",
			Location:    "杭州",
			SocialLinks: `{"github":"https://github.com/example"}`,
			EDCGear:     `[{"name":"机械相机","category":"影像器材"}]`,
		}
		db.DB.Create(&initialProfile)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/profile", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		// 更新 profile (传入顶层 email 与 github 验证 RG-09)
		updatePayload := UpdateCuratorProfileRequest{
			Motto:  "真实生活大于一切",
			Bio:    "安静写字的数字工坊",
			Email:  "hello@example.com",
			Github: "https://github.com/new-curator",
		}
		body, _ := json.Marshal(updatePayload)
		putReq := httptest.NewRequest(http.MethodPut, "/api/v1/profile", bytes.NewReader(body))
		putReq.Header.Set("Content-Type", "application/json")
		putW := httptest.NewRecorder()
		r.ServeHTTP(putW, putReq)

		if putW.Code != http.StatusOK {
			t.Fatalf("expected 200 for update profile, got %d", putW.Code)
		}

		var res struct {
			Data models.CuratorProfile `json:"data"`
		}
		_ = json.Unmarshal(putW.Body.Bytes(), &res)
		if res.Data.Motto != "真实生活大于一切" || res.Data.Bio != "安静写字的数字工坊" {
			t.Fatalf("unexpected updated profile: %+v", res.Data)
		}
		if res.Data.Email != "hello@example.com" || res.Data.Github != "https://github.com/new-curator" {
			t.Fatalf("expected flat email and github in response (RG-09), got email=%q github=%q", res.Data.Email, res.Data.Github)
		}
	})
}

func TestSystemUpdateCheck(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	resetUpdateCheckCache()

	adminToken := "lldt_test_admin_update_token"
	seedTestAdminDevice(adminToken)

	r := gin.New()
	write := r.Group("/api/v1")
	write.Use(middleware.AdminAuthRequired())
	{
		write.GET("/system/update-check", CheckSystemUpdate)
	}

	t.Run("1. Visitors without admin token are rejected with 401 (zero visitor exposure)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/update-check", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized for visitor on update-check, got %d", w.Code)
		}
	})

	t.Run("2. UpdateCheck handles 404 (no releases yet), 200 (newer version + 1h cache), and 500 (graceful check_error)", func(t *testing.T) {
		origVersion := config.Version
		origURL := githubLatestReleaseURL
		defer func() {
			config.Version = origVersion
			githubLatestReleaseURL = origURL
			resetUpdateCheckCache()
		}()

		config.Version = "v1.0.0"
		mockMode := "404"
		hitCount := 0

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hitCount++
			switch mockMode {
			case "404":
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			case "200":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"tag_name": "v1.2.0",
					"html_url": "https://github.com/Losse-Leaf/backend/releases/tag/v1.2.0",
					"published_at": "2026-10-11T10:00:00Z"
				}`))
			case "500":
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		defer mockServer.Close()
		githubLatestReleaseURL = mockServer.URL

		// (a) 404 empty release fallback
		resetUpdateCheckCache()
		req404 := httptest.NewRequest(http.MethodGet, "/api/v1/system/update-check", nil)
		req404.Header.Set("Authorization", "Bearer "+adminToken)
		w404 := httptest.NewRecorder()
		r.ServeHTTP(w404, req404)
		if w404.Code != http.StatusOK {
			t.Fatalf("expected 200 on 404 release fallback, got %d", w404.Code)
		}
		var res404 struct {
			Data UpdateCheckResponse `json:"data"`
		}
		_ = json.Unmarshal(w404.Body.Bytes(), &res404)
		if res404.Data.HasUpdate || res404.Data.LatestVersion != "" || res404.Data.CheckError == "" {
			t.Fatalf("expected 404 to report informative check_error, got: %+v", res404.Data)
		}

		// (b) 200 newer version + 1h cache hit
		resetUpdateCheckCache()
		mockMode = "200"
		beforeHits := hitCount
		req200 := httptest.NewRequest(http.MethodGet, "/api/v1/system/update-check", nil)
		req200.Header.Set("Authorization", "Bearer "+adminToken)
		w200 := httptest.NewRecorder()
		r.ServeHTTP(w200, req200)
		if w200.Code != http.StatusOK {
			t.Fatalf("expected 200 on update check, got %d", w200.Code)
		}
		var res200 struct {
			Data UpdateCheckResponse `json:"data"`
		}
		_ = json.Unmarshal(w200.Body.Bytes(), &res200)
		if !res200.Data.HasUpdate || res200.Data.LatestVersion != "v1.2.0" || res200.Data.CurrentVersion != "v1.0.0" {
			t.Fatalf("expected has_update=true and latest_version=v1.2.0, got %+v", res200.Data)
		}
		if hitCount != beforeHits+1 {
			t.Fatalf("expected 1 upstream hit, got %d", hitCount-beforeHits)
		}

		// Second call within 1h should hit memory cache without calling mockServer
		reqCached := httptest.NewRequest(http.MethodGet, "/api/v1/system/update-check", nil)
		reqCached.Header.Set("Authorization", "Bearer "+adminToken)
		wCached := httptest.NewRecorder()
		r.ServeHTTP(wCached, reqCached)
		if hitCount != beforeHits+1 {
			t.Fatalf("expected cached response without new upstream hit, but hitCount increased")
		}

		// (c) 500 error returns HTTP 200 with check_error
		resetUpdateCheckCache()
		mockMode = "500"
		req500 := httptest.NewRequest(http.MethodGet, "/api/v1/system/update-check", nil)
		req500.Header.Set("Authorization", "Bearer "+adminToken)
		w500 := httptest.NewRecorder()
		r.ServeHTTP(w500, req500)
		if w500.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200 on upstream 500, got %d", w500.Code)
		}
		var res500 struct {
			Data UpdateCheckResponse `json:"data"`
		}
		_ = json.Unmarshal(w500.Body.Bytes(), &res500)
		if res500.Data.HasUpdate || res500.Data.CheckError == "" {
			t.Fatalf("expected has_update=false and non-empty check_error, got %+v", res500.Data)
		}
	})
}
