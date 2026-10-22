package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
)

func TestFilesHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	_ = storage.InitR2(config.R2Config{})

	// 隔离测试运行期间的本地落盘到临时目录 (RC-68)
	tempUploadDir := t.TempDir()
	origDir := storage.GetLocalUploadDir()
	storage.SetLocalUploadDir(tempUploadDir)
	t.Cleanup(func() {
		storage.SetLocalUploadDir(origDir)
	})

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
		// 注册一条合法的生效附件记录 (绑定至未删除的 Entry) (BE-21)
		var testEntry models.LifeEntry
		db.DB.FirstOrCreate(&testEntry, models.LifeEntry{Title: "Test Live Entry"})
		db.DB.Create(&models.Attachment{
			EntryID:          &testEntry.ID,
			ObjectKey:        "entries/2026/10/09/sample.jpg",
			OriginalFilename: "sample.jpg",
			MimeType:         "image/jpeg",
		})

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

	t.Run("本机主存储时批量换签必须走本机媒体签名（BE-45）", func(t *testing.T) {
		// 背景：单发接口 `SignFileURL` 已修（本机为主时走本机媒体签名），
		// 而**批量接口漏了** —— 它只走 R2 预签名，
		// 于是在「R2 只是备份、真存储在 VPS 本地」的部署下
		// `GeneratePresignedGetURL` 报错 ⇒ 回落到**没有签名**的公开地址
		// ⇒ 换到的地址根本访问不了。
		//
		// 这里造一个**本机存在**的对象，验证批量换签给出的是
		// 带签名（exp + sig）的本机媒体地址。
		uploadDir := storage.GetLocalUploadDir()
		relPath := "uploads/batch-sign-test.bin"
		fullPath := filepath.Join(uploadDir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if !storage.ObjectExistsLocally(relPath) {
			t.Fatalf("测试前提不成立：本机应存在 %s", relPath)
		}
		// 关键：批量换签对**非管理员**只允许签「已在系统登记」的对象（BE-21），
		// 所以必须把它登记成一条挂在有效散页上的附件，否则请求会被直接过滤掉。
		entry := models.LifeEntry{
			Type:       "life",
			Title:      "批量换签测试",
			Content:    "测试用",
			OccurredAt: time.Now(),
		}
		if err := db.DB.Create(&entry).Error; err != nil {
			t.Fatalf("create entry: %v", err)
		}
		entryID := entry.ID
		att := models.Attachment{
			EntryID:   &entryID,
			ObjectKey: relPath,
			MimeType:  "application/octet-stream",
			URL:       "/media/" + relPath,
			CreatedAt: time.Now(),
		}
		if err := db.DB.Create(&att).Error; err != nil {
			t.Fatalf("create attachment: %v", err)
		}

		body, _ := json.Marshal(map[string]interface{}{"keys": []string{relPath}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/files/sign-batch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp Response
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		dataMap := resp.Data.(map[string]interface{})
		items := dataMap["items"].([]interface{})
		if len(items) != 1 {
			t.Fatalf("expected 1 signed item, got %d", len(items))
		}
		item := items[0].(map[string]interface{})
		url, _ := item["url"].(string)
		if url == "" {
			t.Fatalf("批量换签必须给出非空 url，实际为空")
		}
		if !strings.Contains(url, "sig=") || !strings.Contains(url, "exp=") {
			t.Errorf("本机媒体地址必须带签名参数（exp + sig），实际: %s", url)
		}
		// 缩略图地址也必须一并给出（客户端没法自己拼 —— 签名密钥只在服务端）
		thumb, _ := item["thumbnail_url"].(string)
		if thumb == "" {
			t.Errorf("批量换签必须一并给出缩略图签名地址")
		} else if !strings.Contains(thumb, "sig=") {
			t.Errorf("缩略图地址也必须带签名参数，实际: %s", thumb)
		}
	})

	t.Run("BE-21 Public visitor signing oracle prevention", func(t *testing.T) {
		// 1. 访客尝试对未在系统登记的游离 object_key 签名，应被 403 拦截
		unregisteredKey := "system/confidential_backup.sql"
		req := httptest.NewRequest(http.MethodGet, "/api/v1/files/sign?key="+unregisteredKey, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for unregistered key, got %d: %s", w.Code, w.Body.String())
		}

		// 2. 访客批量签名时，未登记的游离 key 应被静默过滤
		batchPayload := BatchSignRequest{
			Keys: []string{"avatar/user.png", unregisteredKey},
		}
		batchBody, _ := json.Marshal(batchPayload)
		batchReq := httptest.NewRequest(http.MethodPost, "/api/v1/files/sign-batch", bytes.NewReader(batchBody))
		batchReq.Header.Set("Content-Type", "application/json")
		batchW := httptest.NewRecorder()
		r.ServeHTTP(batchW, batchReq)

		if batchW.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", batchW.Code)
		}
		var batchResp Response
		_ = json.Unmarshal(batchW.Body.Bytes(), &batchResp)
		batchMap := batchResp.Data.(map[string]interface{})
		batchItems := batchMap["items"].([]interface{})
		if len(batchItems) != 1 {
			t.Fatalf("expected 1 item (unregistered key filtered out), got %d", len(batchItems))
		}

		// 3. 管理员授权请求对未登记 key 签名，应放行 200
		adminToken := "lldt_test_admin_signing_token_12345"
		seedTestAdminDevice(adminToken)

		adminReq := httptest.NewRequest(http.MethodGet, "/api/v1/files/sign?key="+unregisteredKey, nil)
		adminReq.Header.Set("Authorization", "Bearer "+adminToken)
		adminW := httptest.NewRecorder()
		r.ServeHTTP(adminW, adminReq)

		if adminW.Code != http.StatusOK {
			t.Fatalf("expected 200 for authenticated admin signing, got %d: %s", adminW.Code, adminW.Body.String())
		}

		// 4. 已解绑附件 (entry_id/wish_id/footprint_id 均为 nil) 访客禁止签名 (403)
		unboundKey := "entries/2026/10/09/unbound_photo.jpg"
		db.DB.Create(&models.Attachment{
			ObjectKey: unboundKey,
		})
		unboundReq := httptest.NewRequest(http.MethodGet, "/api/v1/files/sign?key="+unboundKey, nil)
		unboundW := httptest.NewRecorder()
		r.ServeHTTP(unboundW, unboundReq)
		if unboundW.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for unbound attachment, got %d", unboundW.Code)
		}

		// 5. 软删除实体的关联附件，访客禁止签名 (403)
		deletedEntry := models.LifeEntry{Title: "Deleted Entry"}
		db.DB.Create(&deletedEntry)
		softDelKey := "entries/2026/10/09/soft_deleted_parent.jpg"
		db.DB.Create(&models.Attachment{
			EntryID:   &deletedEntry.ID,
			ObjectKey: softDelKey,
		})
		// 软删除该 Entry
		db.DB.Delete(&deletedEntry)

		softDelReq := httptest.NewRequest(http.MethodGet, "/api/v1/files/sign?key="+softDelKey, nil)
		softDelW := httptest.NewRecorder()
		r.ServeHTTP(softDelW, softDelReq)
		if softDelW.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for attachment of soft-deleted entry, got %d", softDelW.Code)
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
			AvatarURL: "https://media.example.com/avatar/user.png?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=987654",
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
