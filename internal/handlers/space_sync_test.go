package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

func TestVesperSpaceDataLayerProjectionAndAlistMirror(t *testing.T) {
	setupTestDB(t)

	tmpDir := t.TempDir()
	oldDir := storage.GetLocalUploadDir()
	storage.SetLocalUploadDir(tmpDir)
	defer func() {
		storage.SetLocalUploadDir(oldDir)
		storage.InvalidateAllLocalObjectCache()
	}()

	var mu sync.Mutex
	alistFiles := make(map[string][]byte)
	spaceWorks := make(map[string]map[string]any)
	var deletedWorks []string

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.URL.Path == "/alist/api/fs/put" && r.Method == http.MethodPut:
			fp := r.Header.Get("File-Path")
			raw, _ := io.ReadAll(r.Body)
			alistFiles[fp] = raw
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))

		case strings.HasPrefix(r.URL.Path, "/alist/d/space/looseleaf/") && r.Method == http.MethodGet:
			for k, v := range alistFiles {
				if strings.Contains(k, "sunset.jpg") {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(v)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		case r.URL.Path == "/space/api/v1/works" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["id"].(string)
			spaceWorks[id] = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"work":{"id":"` + id + `"}}`))

		case strings.HasPrefix(r.URL.Path, "/space/api/v1/works/") && r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/space/api/v1/works/")
			delete(spaceWorks, id)
			deletedWorks = append(deletedWorks, id)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer mockSrv.Close()

	t.Setenv("ALIST_BASE_URL", mockSrv.URL+"/alist")
	t.Setenv("ALIST_TOKEN", "alist-test-token")
	t.Setenv("ALIST_MOUNT_PATH", "/space/looseleaf")
	t.Setenv("VESPER_SPACE_URL", mockSrv.URL+"/space")
	t.Setenv("INTERNAL_BRIDGE_TOKEN", "ll_brg_test_secret")

	// 1. Upload an image object -> saved locally and mirrored to AList
	objKey := "entries/2026/11/23/test-sunset.jpg"
	imgBytes := []byte("fake-jpeg-binary-data-12345")
	if err := storage.UploadObject(context.Background(), objKey, strings.NewReader(string(imgBytes)), int64(len(imgBytes)), "image/jpeg"); err != nil {
		t.Fatalf("UploadObject failed: %v", err)
	}

	att := models.Attachment{
		ObjectKey:        objKey,
		OriginalFilename: "sunset.jpg",
		MimeType:         "image/jpeg",
		Size:             int64(len(imgBytes)),
		Width:            1600,
		Height:           1200,
	}
	if err := db.DB.Create(&att).Error; err != nil {
		t.Fatalf("create attachment: %v", err)
	}

	entry := models.LifeEntry{
		Type:       "life",
		Title:      "太平洋落日",
		Content:    "海风很轻，云层像金箔一样铺开。",
		Location:   "Santa Monica",
		OccurredAt: time.Date(2026, 11, 23, 17, 30, 0, 0, time.UTC),
		IsPrivate:  false,
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatalf("create entry: %v", err)
	}
	db.DB.Model(&att).Update("entry_id", entry.ID)

	// 2. Project entry to Vesper Space
	if err := SyncEntryToVesperSpace(context.Background(), entry.ID, nil); err != nil {
		t.Fatalf("SyncEntryToVesperSpace create failed: %v", err)
	}

	expectedWorkID := spaceWorkIDForAttachment(att.ID)
	mu.Lock()
	projected, ok := spaceWorks[expectedWorkID]
	mu.Unlock()
	if !ok {
		t.Fatalf("expected work %s to be projected to Vesper Space", expectedWorkID)
	}
	if projected["title"] != "太平洋落日" || projected["category"] != "Travel" || projected["visibility"] != "public" {
		t.Fatalf("unexpected projected work metadata: %+v", projected)
	}
	if projected["aspect"] != 1.33 {
		t.Fatalf("expected aspect 1.33 from 1600x1200, got %v", projected["aspect"])
	}

	// 3. Toggle entry to private -> automatic projection updates visibility to private
	db.DB.Model(&entry).Update("is_private", true)
	if err := SyncEntryToVesperSpace(context.Background(), entry.ID, nil); err != nil {
		t.Fatalf("SyncEntryToVesperSpace update failed: %v", err)
	}
	mu.Lock()
	projectedPriv := spaceWorks[expectedWorkID]
	mu.Unlock()
	if projectedPriv["visibility"] != "private" {
		t.Fatalf("expected private entry to project visibility=private, got %v", projectedPriv["visibility"])
	}

	// 4. Transparent fallback: remove local file, ReadObject pulls from AList /space/looseleaf and re-caches locally
	_ = storage.DeleteLocalObject(objKey)
	if storage.ObjectExistsLocally(objKey) {
		t.Fatalf("expected local object to be deleted before fallback test")
	}
	recovered, rErr := storage.ReadObject(context.Background(), objKey)
	if rErr != nil || string(recovered) != string(imgBytes) {
		t.Fatalf("expected ReadObject to transparently recover from AList 5TB mirror, err=%v got=%q", rErr, string(recovered))
	}
	if !storage.ObjectExistsLocally(objKey) {
		t.Fatalf("expected recovered object to be re-cached on local SSD")
	}

	// 5. Delete entry -> removes projected work from Vesper Space
	db.DB.Delete(&entry)
	if err := SyncEntryToVesperSpace(context.Background(), entry.ID, nil); err != nil {
		t.Fatalf("SyncEntryToVesperSpace delete failed: %v", err)
	}
	mu.Lock()
	_, stillExists := spaceWorks[expectedWorkID]
	mu.Unlock()
	if stillExists || len(deletedWorks) == 0 {
		t.Fatalf("expected deleted entry to remove work %s from Vesper Space", expectedWorkID)
	}
}
