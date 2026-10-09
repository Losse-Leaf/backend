package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"chimu-lab/internal/config"
	"chimu-lab/internal/db"
	"chimu-lab/internal/models"
	"chimu-lab/internal/storage"

	"github.com/gin-gonic/gin"
)

func TestFilesHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	_ = storage.InitR2(config.R2Config{})

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		v1.GET("/files/sign", SignFileURL)
		v1.POST("/files/sign-batch", SignFilesBatch)
		v1.POST("/files/upload", UnifiedUploadFile)
		v1.GET("/profile", GetCuratorProfile)
		v1.PUT("/profile", UpdateCuratorProfile)
	}

	t.Run("Sign single file with default 30m expiration", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/files/sign?key=avatar/user.png", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp Response
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		dataMap, ok := resp.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("expected data map, got %T", resp.Data)
		}

		if dataMap["object_key"] != "avatar/user.png" {
			t.Errorf("expected object_key 'avatar/user.png', got '%v'", dataMap["object_key"])
		}
		if dataMap["url"] == "" {
			t.Errorf("expected non-empty url")
		}
		if int(dataMap["expires_in"].(float64)) != 1800 {
			t.Errorf("expected default 1800s expires_in, got %v", dataMap["expires_in"])
		}
	})

	t.Run("Sign single file with custom expiration", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/files/sign?key=avatar/user.png&expires=600", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var resp Response
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		dataMap := resp.Data.(map[string]interface{})

		if int(dataMap["expires_in"].(float64)) != 600 {
			t.Errorf("expected 600s expires_in, got %v", dataMap["expires_in"])
		}
	})

	t.Run("Sign file with missing key returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/files/sign", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("Batch sign files", func(t *testing.T) {
		payload := BatchSignRequest{
			Keys: []string{"avatar/user.png", "entries/2026/10/09/sample.jpg"},
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/files/sign-batch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		var resp Response
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		dataMap := resp.Data.(map[string]interface{})
		items := dataMap["items"].([]interface{})
		if len(items) != 2 {
			t.Fatalf("expected 2 signed items, got %d", len(items))
		}
	})

	t.Run("Unified upload file multipart", func(t *testing.T) {
		bodyBuf := &bytes.Buffer{}
		writer := multipart.NewWriter(bodyBuf)

		// file form field
		part, err := writer.CreateFormFile("file", "test_photo.png")
		if err != nil {
			t.Fatalf("failed to create form file: %v", err)
		}
		_, _ = io.WriteString(part, "fake-image-content-for-testing")

		// folder field
		_ = writer.WriteField("folder", "avatar")
		_ = writer.WriteField("key", "avatar/user.png")
		_ = writer.Close()

		req := httptest.NewRequest(http.MethodPost, "/api/v1/files/upload", bodyBuf)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp Response
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		dataMap := resp.Data.(map[string]interface{})

		if dataMap["object_key"] != "avatar/user.png" {
			t.Errorf("expected object_key 'avatar/user.png', got '%v'", dataMap["object_key"])
		}
		if dataMap["url"] == "" {
			t.Errorf("expected url in upload response")
		}
		if int(dataMap["expires_in"].(float64)) != 1800 {
			t.Errorf("expected 1800s expires_in, got %v", dataMap["expires_in"])
		}

		// Verify Attachment record was created in database
		var att models.Attachment
		if err := db.DB.Where("object_key = ?", "avatar/user.png").First(&att).Error; err != nil {
			t.Errorf("failed to find attachment record in db: %v", err)
		}
	})

	t.Run("CuratorProfile avatar relative path preservation and signed URL generation", func(t *testing.T) {
		// Update profile with relative path
		updateReq := UpdateCuratorProfileRequest{
			AvatarKey: "avatar/user.png",
		}
		body, _ := json.Marshal(updateReq)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/profile", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}

		// Fetch profile
		getReq := httptest.NewRequest(http.MethodGet, "/api/v1/profile", nil)
		getW := httptest.NewRecorder()
		r.ServeHTTP(getW, getReq)

		if getW.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", getW.Code)
		}

		var resp Response
		_ = json.Unmarshal(getW.Body.Bytes(), &resp)
		pMap := resp.Data.(map[string]interface{})

		if pMap["avatar_key"] != "avatar/user.png" {
			t.Errorf("expected avatar_key 'avatar/user.png', got '%v'", pMap["avatar_key"])
		}
		if pMap["avatar_url"] == "" || pMap["avatar_url"] == "avatar/user.png" {
			t.Errorf("expected avatar_url to be dynamic signed URL, got '%v'", pMap["avatar_url"])
		}

		// Update profile with full presigned URL and verify it extracts clean relative path
		updateReqWithFullURL := UpdateCuratorProfileRequest{
			AvatarURL: "https://assets.codeactivityhub.top/avatar/user.png?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=987654",
		}
		body2, _ := json.Marshal(updateReqWithFullURL)
		req2 := httptest.NewRequest(http.MethodPut, "/api/v1/profile", bytes.NewReader(body2))
		req2.Header.Set("Content-Type", "application/json")
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, req2)

		if w2.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w2.Code)
		}

		// Verify database only stored the clean relative path, not the expiring query string!
		var profile models.CuratorProfile
		db.DB.First(&profile)
		if profile.AvatarURL != "avatar/user.png" {
			t.Errorf("expected clean DB AvatarURL 'avatar/user.png', got '%s'", profile.AvatarURL)
		}
	})
}
