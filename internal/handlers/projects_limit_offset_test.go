package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

func TestGetProjectsV1LimitZeroAndOffset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	enableAllPublicModulesForTest(t)

	// Seed 505 projects in batch
	projects := make([]models.Project, 505)
	for i := 0; i < 505; i++ {
		projects[i] = models.Project{
			Title:  fmt.Sprintf("Project %04d", i+1),
			Status: "进行中",
			Order:  i,
		}
	}
	if err := db.DB.CreateInBatches(projects, 100).Error; err != nil {
		t.Fatalf("failed to seed projects: %v", err)
	}

	r := gin.New()
	r.GET("/api/v1/projects", GetProjectsV1)

	// 1. Default request: should return default 500
	{
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", w.Code)
		}
		var resp struct {
			Code int              `json:"code"`
			Data []models.Project `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}
		if len(resp.Data) != 500 {
			t.Errorf("expected 500 projects with default limit, got %d", len(resp.Data))
		}
	}

	// 2. limit=0: should return all 505 projects without truncation (OPT-01)
	{
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/projects?limit=0", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", w.Code)
		}
		var resp struct {
			Code int              `json:"code"`
			Data []models.Project `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}
		if len(resp.Data) != 505 {
			t.Errorf("expected 505 projects with limit=0, got %d", len(resp.Data))
		}
	}

	// 3. offset=500: should return the remaining 5 projects
	{
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/projects?offset=500", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", w.Code)
		}
		var resp struct {
			Code int              `json:"code"`
			Data []models.Project `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}
		if len(resp.Data) != 5 {
			t.Errorf("expected 5 projects with offset=500, got %d", len(resp.Data))
		}
	}
}
