package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

func TestCreatePhotoOnlyEntryAndIdempotentCompleteUpload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	enableAllPublicModulesForTest(t)

	r := gin.New()
	r.POST("/api/v1/uploads/complete", CompleteUpload)
	r.POST("/api/v1/entries", CreateLifeEntry)

	completePayload := CompleteUploadRequest{
		UploadID:         "ab12cd34-upload-id",
		ObjectKey:        "entries/2026/11/26/ab12cd34-photo.jpg",
		OriginalFilename: "photo.jpg",
		MimeType:         "image/jpeg",
		Size:             102400,
		Width:            1600,
		Height:           1200,
	}
	completeBytes, _ := json.Marshal(completePayload)

	// First CompleteUpload
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodPost, "/api/v1/uploads/complete", bytes.NewBuffer(completeBytes))
	req1.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first CompleteUpload failed: %d %s", w1.Code, w1.Body.String())
	}
	var res1 struct {
		Data models.Attachment `json:"data"`
	}
	if err := json.Unmarshal(w1.Body.Bytes(), &res1); err != nil {
		t.Fatalf("decode first CompleteUpload failed: %v", err)
	}
	if res1.Data.ID == 0 {
		t.Fatalf("expected non-zero attachment ID")
	}

	// Second CompleteUpload with identical object_key must be idempotent
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodPost, "/api/v1/uploads/complete", bytes.NewBuffer(completeBytes))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second CompleteUpload failed: %d %s", w2.Code, w2.Body.String())
	}
	var res2 struct {
		Data models.Attachment `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &res2); err != nil {
		t.Fatalf("decode second CompleteUpload failed: %v", err)
	}
	if res2.Data.ID != res1.Data.ID {
		t.Fatalf("expected idempotent attachment ID %d, got %d", res1.Data.ID, res2.Data.ID)
	}

	// Photo-only entry (empty title and empty content) must succeed
	entryBytes, _ := json.Marshal(CreateEntryRequest{
		Type:          "life",
		Title:         "",
		Content:       "",
		Location:      "上海市 · 延安东路",
		AttachmentIDs: []uint{res1.Data.ID},
	})
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(entryBytes))
	req3.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("photo-only CreateLifeEntry failed: %d %s", w3.Code, w3.Body.String())
	}
	var res3 struct {
		Data models.LifeEntry `json:"data"`
	}
	if err := json.Unmarshal(w3.Body.Bytes(), &res3); err != nil {
		t.Fatalf("decode photo-only CreateLifeEntry failed: %v", err)
	}
	if res3.Data.Content != "" || res3.Data.Title != "" {
		t.Fatalf("expected empty content and title, got content=%q title=%q", res3.Data.Content, res3.Data.Title)
	}
	if len(res3.Data.Attachments) != 1 || res3.Data.Attachments[0].ID != res1.Data.ID {
		t.Fatalf("expected bound attachment %d, got %+v", res1.Data.ID, res3.Data.Attachments)
	}

	// Empty title, empty content, and no attachments must still fail with 400
	emptyBytes, _ := json.Marshal(CreateEntryRequest{
		Type:    "life",
		Title:   "",
		Content: "   ",
	})
	w4 := httptest.NewRecorder()
	req4, _ := http.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewBuffer(emptyBytes))
	req4.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for completely empty entry, got %d: %s", w4.Code, w4.Body.String())
	}

	var count int64
	db.DB.Model(&models.Attachment{}).Where("object_key = ?", completePayload.ObjectKey).Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 attachment row for object_key, got %d", count)
	}
}
