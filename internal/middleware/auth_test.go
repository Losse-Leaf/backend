package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test in-memory db: %v", err)
	}
	_ = database.AutoMigrate(&models.Device{}, &models.PairTicket{})
	db.DB = database
}

func TestAdminAuthRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)

	t.Run("Passes when ADMIN_API_KEY is empty", func(t *testing.T) {
		config.AppConfig.AdminAPIKey = ""
		r := gin.New()
		r.Use(AdminAuthRequired())
		r.POST("/test", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})

		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("Rejects unauthorized request when ADMIN_API_KEY is configured", func(t *testing.T) {
		config.AppConfig.AdminAPIKey = "secret-test-token"
		r := gin.New()
		r.Use(AdminAuthRequired())
		r.POST("/test", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})

		// 1. 无 header
		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}

		// 2. 错误 token
		reqErr := httptest.NewRequest(http.MethodPost, "/test", nil)
		reqErr.Header.Set("Authorization", "Bearer wrong-token")
		wErr := httptest.NewRecorder()
		r.ServeHTTP(wErr, reqErr)
		if wErr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for wrong token, got %d", wErr.Code)
		}

		// 3. 正确 token (Bearer)
		reqOk := httptest.NewRequest(http.MethodPost, "/test", nil)
		reqOk.Header.Set("Authorization", "Bearer secret-test-token")
		wOk := httptest.NewRecorder()
		r.ServeHTTP(wOk, reqOk)
		if wOk.Code != http.StatusOK {
			t.Errorf("expected 200 for correct token, got %d", wOk.Code)
		}

		// 4. 正确 token (X-Device-Token)
		reqDev := httptest.NewRequest(http.MethodPost, "/test", nil)
		reqDev.Header.Set("X-Device-Token", "secret-test-token")
		wDev := httptest.NewRecorder()
		r.ServeHTTP(wDev, reqDev)
		if wDev.Code != http.StatusOK {
			t.Errorf("expected 200 for X-Device-Token, got %d", wDev.Code)
		}
	})

	t.Run("Accepts valid paired Device Token", func(t *testing.T) {
		config.AppConfig.AdminAPIKey = "master-super-secret"
		rawDeviceToken := "lldt_live_test_device_token_12345"
		tokenHash := hashToken(rawDeviceToken)

		device := models.Device{
			DeviceID:     "dev_ios_test1",
			Name:         "iPhone Test",
			Platform:     "ios",
			TokenHash:    tokenHash,
			Status:       "active",
			PairedAt:     time.Now(),
			LastActiveAt: time.Now(),
		}
		db.DB.Create(&device)

		r := gin.New()
		r.Use(AdminAuthRequired())
		r.POST("/test", func(c *gin.Context) {
			authType, _ := c.Get("auth_type")
			devID, _ := c.Get("device_id")
			c.JSON(http.StatusOK, gin.H{
				"auth_type": authType,
				"device_id": devID,
			})
		})

		// 1. 使用该设备的 device token
		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		req.Header.Set("Authorization", "Bearer "+rawDeviceToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for paired device token, got %d", w.Code)
		}

		// 2. 吊销设备后再次请求应被拦截
		db.DB.Model(&device).Update("status", "revoked")
		reqRevoked := httptest.NewRequest(http.MethodPost, "/test", nil)
		reqRevoked.Header.Set("Authorization", "Bearer "+rawDeviceToken)
		wRevoked := httptest.NewRecorder()
		r.ServeHTTP(wRevoked, reqRevoked)

		if wRevoked.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for revoked device token, got %d", wRevoked.Code)
		}
	})
}
