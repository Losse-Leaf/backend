package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// MOB-P1-xx：Service Worker 的文件必须能按**原路径**取到，不能落进 SPA 回退。
//
// 踩过的坑：本地磁盘分支只白名单了 favicon / manifest / robots 几个文件，
// 于是 /registerSW.js、/sw.js、/workbox-xxx.js 全部走 NoRoute ⇒ 返回 index.html
// （HTML！）⇒ 浏览器报 "SyntaxError: Unexpected token '<'"
// ⇒ **Service Worker 注册失败**，PWA 的离线能力整个不生效。
//
// 这条测试守住「这些路径返回的是 JS/图标，不是 HTML」。
func TestFrontendStaticServingServiceWorkerAssets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	setupFrontendStaticServing(r)

	// 这些路径在**嵌入副本**里都存在（backend/web/dist 由 //go:embed 带入），
	// 因此无论走本地磁盘还是嵌入分支，都必须能被正确取到。
	cases := []struct {
		path        string
		contentType string
	}{
		{"/registerSW.js", "javascript"},
		{"/sw.js", "javascript"},
		{"/manifest.webmanifest", "json"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", tc.path, w.Code)
			continue
		}
		ct := w.Header().Get("Content-Type")
		if !strings.Contains(ct, tc.contentType) {
			t.Errorf("%s: expected content-type containing %q, got %q", tc.path, tc.contentType, ct)
		}
		body := w.Body.String()
		if strings.HasPrefix(strings.TrimSpace(body), "<") {
			t.Errorf("%s: got HTML (SPA fallback) instead of the real asset — SW registration would fail", tc.path)
		}
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
