package handlers

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"loose-leaf/internal/backup"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"

	"github.com/gin-gonic/gin"
)

func TestBackupHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tempDir, err := os.MkdirTemp("", "looseleaf-handlers-backup-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	testDBPath := filepath.Join(tempDir, "looseleaf.db")
	config.AppConfig.DBPath = testDBPath

	_, err = db.InitDB(testDBPath)
	if err != nil {
		t.Fatalf("Failed to init test db: %v", err)
	}

	r := gin.New()
	api := r.Group("/api/v1/system/backup")
	{
		api.POST("/now", CreateBackupNow)
		api.GET("/list", GetBackupList)
		api.GET("/download/:filename", DownloadBackup)
		api.POST("/restore", RestoreBackup)
	}

	// 1. 测试立即备份
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/system/backup/now", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("CreateBackupNow failed with code %d, body: %s", w.Code, w.Body.String())
	}

	var createResp struct {
		Data backup.BackupItem `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("Failed to decode create backup response: %v", err)
	}
	if createResp.Data.Filename == "" {
		t.Fatalf("Expected valid filename in backup response")
	}

	// 2. 测试获取备份列表
	wList := httptest.NewRecorder()
	reqList, _ := http.NewRequest("GET", "/api/v1/system/backup/list", nil)
	r.ServeHTTP(wList, reqList)

	if wList.Code != http.StatusOK {
		t.Fatalf("GetBackupList failed with code %d", wList.Code)
	}
	var listResp struct {
		Data []backup.BackupItem `json:"data"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("Failed to decode backup list: %v", err)
	}
	if len(listResp.Data) == 0 {
		t.Fatalf("Expected non-empty backup list")
	}

	// 3. 测试下载备份
	wDl := httptest.NewRecorder()
	reqDl, _ := http.NewRequest("GET", "/api/v1/system/backup/download/"+createResp.Data.Filename, nil)
	r.ServeHTTP(wDl, reqDl)

	if wDl.Code != http.StatusOK {
		t.Fatalf("DownloadBackup failed with code %d", wDl.Code)
	}
	if wDl.Header().Get("Content-Type") != "application/zip" {
		t.Errorf("Expected application/zip Content-Type, got %s", wDl.Header().Get("Content-Type"))
	}
	zipBytes := wDl.Body.Bytes()
	if len(zipBytes) == 0 {
		t.Fatalf("Downloaded zip is empty")
	}

	// 4. 测试 JSON 模式远程回滚
	restorePayload, _ := json.Marshal(map[string]string{
		"filename": createResp.Data.Filename,
	})
	wRestore := httptest.NewRecorder()
	reqRestore, _ := http.NewRequest("POST", "/api/v1/system/backup/restore", bytes.NewBuffer(restorePayload))
	reqRestore.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wRestore, reqRestore)

	if wRestore.Code != http.StatusOK {
		t.Fatalf("RestoreBackup via filename failed with code %d, body: %s", wRestore.Code, wRestore.Body.String())
	}

	// 5. 测试上传文件模式还原
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", createResp.Data.Filename)
	if err != nil {
		t.Fatalf("Failed to create form file: %v", err)
	}
	_, _ = part.Write(zipBytes)
	writer.Close()

	wUploadRestore := httptest.NewRecorder()
	reqUploadRestore, _ := http.NewRequest("POST", "/api/v1/system/backup/restore", body)
	reqUploadRestore.Header.Set("Content-Type", writer.FormDataContentType())
	r.ServeHTTP(wUploadRestore, reqUploadRestore)

	if wUploadRestore.Code != http.StatusOK {
		t.Fatalf("RestoreBackup via upload failed with code %d, body: %s", wUploadRestore.Code, wUploadRestore.Body.String())
	}
}
