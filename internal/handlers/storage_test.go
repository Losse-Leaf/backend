package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
)

func setupStorageTestRouter(t *testing.T, adminToken string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	setupTestFullDB(t)
	seedTestAdminDevice(adminToken)

	siteCfg := models.SiteConfig{
		SiteName:    "Loose-Leaf Storage Test",
		Initialized: true,
	}
	db.DB.Create(&siteCfg)

	r := gin.New()
	v1 := r.Group("/api/v1")
	v1.GET("/storage/status", GetStorageStatus)

	write := v1.Group("")
	write.Use(middleware.AdminAuthRequired())
	{
		write.POST("/storage/test", TestStorageConfig)
		write.PUT("/storage/config", UpdateStorageConfig)
		write.DELETE("/storage/config", ResetStorageConfig)
	}

	return r
}

func TestStorageEndpoints(t *testing.T) {
	adminToken := "lldt_storage_test_admin_token"
	r := setupStorageTestRouter(t, adminToken)

	t.Run("1. Unauthorized visitor cannot access storage management endpoints", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/storage/test", bytes.NewBufferString("{}"))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401 Unauthorized for test, got %d", w.Code)
		}

		req2, _ := http.NewRequest(http.MethodPut, "/api/v1/storage/config", bytes.NewBufferString("{}"))
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, req2)
		if w2.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401 Unauthorized for put, got %d", w2.Code)
		}

		req3, _ := http.NewRequest(http.MethodDelete, "/api/v1/storage/config", nil)
		w3 := httptest.NewRecorder()
		r.ServeHTTP(w3, req3)
		if w3.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401 Unauthorized for delete, got %d", w3.Code)
		}
	})

	t.Run("2. Validation error on missing required fields", func(t *testing.T) {
		payload := StorageConfigRequest{
			BucketName: "",
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/storage/test", bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 Bad Request, got %d", w.Code)
		}
	})

	t.Run("3. Reset storage config reverts to local storage mode", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, "/api/v1/storage/config", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}

		var res struct {
			Code int                    `json:"code"`
			Data map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if res.Data["configured"] != false {
			t.Errorf("Expected configured=false after reset, got %v", res.Data["configured"])
		}

		// Verify database has R2Disabled=true
		var siteCfg models.SiteConfig
		if err := db.DB.First(&siteCfg).Error; err != nil {
			t.Fatalf("Failed to read site config: %v", err)
		}
		if !siteCfg.R2Disabled {
			t.Errorf("Expected siteCfg.R2Disabled=true, got false")
		}

		// Verify LoadStorageConfigFromDB respects R2Disabled
		LoadStorageConfigFromDB()
		if storage.IsConfigured() {
			t.Errorf("Expected storage.IsConfigured()=false after LoadStorageConfigFromDB()")
		}
	})
}
