package middleware

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_mw.db")
	database, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test in-memory db: %v", err)
	}
	_ = database.AutoMigrate(&models.Device{}, &models.PairTicket{}, &models.SiteConfig{})
	db.DB = database
}

func TestAdminAuthRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)

	t.Run("Rejects every protected route when system is uninitialized (no cold-start bypass)", func(t *testing.T) {
		db.DB.Exec("DELETE FROM site_configs")
		db.DB.Exec("DELETE FROM devices")

		r := gin.New()
		r.Use(AdminAuthRequired())
		r.POST("/api/v1/entries", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})
		r.POST("/api/v1/auth/pair/init", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "bootstrap_pair_ok"})
		})

		// 未初始化时不存在任何免密旁路：常规写接口与配对接口都必须 401。
		for _, path := range []string{"/api/v1/entries", "/api/v1/auth/pair/init"} {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 on %s when uninitialized, got %d", path, w.Code)
			}
		}
	})

	t.Run("Rejects unauthorized request when system is initialized (BE-13, BE-18)", func(t *testing.T) {
		db.DB.Create(&models.SiteConfig{Initialized: true})
		defer func() {
			db.DB.Exec("DELETE FROM site_configs")
		}()

		r := gin.New()
		r.Use(AdminAuthRequired())
		r.POST("/api/v1/auth/pair/init", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/init", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 on initialized system without credentials, got %d", w.Code)
		}
	})

	t.Run("Accepts valid paired Device Token via Bearer or X-Device-Token and rejects when revoked (BE-18)", func(t *testing.T) {
		rawDeviceToken := "lldt_8390abff0192eac881273948bdf12901"
		tokenHash := HashToken(rawDeviceToken)

		device := models.Device{
			DeviceID:     "dev_ios_20261010_48b1",
			Name:         "iPhone 16 Pro",
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
				"is_admin":  IsAdmin(c),
			})
		})

		// 1. Authorization: Bearer <token>
		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		req.Header.Set("Authorization", "Bearer "+rawDeviceToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for Bearer device token, got %d", w.Code)
		}

		// 2. X-Device-Token: <token>
		reqDev := httptest.NewRequest(http.MethodPost, "/test", nil)
		reqDev.Header.Set("X-Device-Token", rawDeviceToken)
		wDev := httptest.NewRecorder()
		r.ServeHTTP(wDev, reqDev)
		if wDev.Code != http.StatusOK {
			t.Fatalf("expected 200 for X-Device-Token, got %d", wDev.Code)
		}

		// 3. 错误 token 返回 401
		reqWrong := httptest.NewRequest(http.MethodPost, "/test", nil)
		reqWrong.Header.Set("Authorization", "Bearer wrong_token")
		wWrong := httptest.NewRecorder()
		r.ServeHTTP(wWrong, reqWrong)
		if wWrong.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for wrong token, got %d", wWrong.Code)
		}

		// 4. 吊销设备后再次请求应被拦截
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
