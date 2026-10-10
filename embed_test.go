package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestFrontendStaticServingAPIIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// Register a dummy API endpoint
	r.GET("/api/v1/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})

	setupFrontendStaticServing(r)

	// 1. Existing API should return 200
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/v1/ping, got %d", w.Code)
	}

	// 2. Non-existent API should return 404 JSON, NOT SPA HTML
	req404 := httptest.NewRequest(http.MethodGet, "/api/v1/non-existent-endpoint", nil)
	w404 := httptest.NewRecorder()
	r.ServeHTTP(w404, req404)
	if w404.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for /api/v1/non-existent-endpoint, got %d", w404.Code)
	}
	if w404.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("expected application/json, got %s", w404.Header().Get("Content-Type"))
	}
}

func TestFrontendStaticServingSPAFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	setupFrontendStaticServing(r)

	// Request root or non-API route
	req := httptest.NewRequest(http.MethodGet, "/timeline", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// If disk or embedded frontend is present, verify HTTP 200 and no-cache header
	if w.Code == http.StatusOK {
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("expected Cache-Control: no-cache, got %s", cc)
		}
	} else if w.Code != http.StatusNotFound {
		t.Fatalf("unexpected status code: %d", w.Code)
	}
}
