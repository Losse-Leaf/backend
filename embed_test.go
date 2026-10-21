package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

		if w.Code == http.StatusNotFound {
			// 当前环境里本来就没有这份产物（CI 的嵌入副本只提交了 .gitkeep）——
			// 404 是合理的；但**绝不能**是 HTML。
			// 真正要验证路由的那条用例见 TestFrontendStaticServingLocalDiskRoutes。
			continue
		}
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

// TestFrontendStaticServingLocalDiskRoutes 在**真实的本地磁盘 dist** 上验证路由。
//
// 上面那条用例依赖「构建产物存在」，CI 里跑不了；这条自己造一个最小 dist，
// 把问题还原成最初的样子：
//   - backend/web/dist 目录恒存在（仓库里只有 .gitkeep）⇒ 旧逻辑永远命中它，
//     于是一份**陈旧拷贝**会遮蔽 ../web/dist 里刚构建出来的新版本
//   - 只白名单了 favicon / manifest ⇒ /registerSW.js、/sw.js 落进 SPA 回退，
//     返回 index.html ⇒ 浏览器 "Unexpected token '<'" ⇒ Service Worker 注册失败
func TestFrontendStaticServingLocalDiskRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 在一个临时工作目录里造出「backend 仓库」与「仓库根的 web/dist」两处目录
	root := t.TempDir()
	backendDir := filepath.Join(root, "backend")
	webDist := filepath.Join(root, "web", "dist")
	placeholder := filepath.Join(backendDir, "web", "dist") // 只有 .gitkeep 的占位目录
	if err := os.MkdirAll(placeholder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(webDist, 0o755); err != nil {
		t.Fatal(err)
	}
	// 真正的构建产物在 ../web/dist，backend/web/dist 只是占位
	writeFile := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(webDist, "index.html"), "<!doctype html><title>fresh</title>")
	writeFile(filepath.Join(webDist, "registerSW.js"), "export const x=1;")
	writeFile(filepath.Join(webDist, "sw.js"), "self.addEventListener('fetch',()=>{});")
	writeFile(filepath.Join(webDist, "manifest.webmanifest"), `{"name":"x"}`)
	writeFile(filepath.Join(webDist, "assets", "app.js"), "console.log(1);")
	writeFile(filepath.Join(webDist, "icons", "icon-192.png"), "PNG")
	writeFile(filepath.Join(webDist, "fonts", "f.woff2"), "wOFF2")
	writeFile(filepath.Join(webDist, "workbox-abc.js"), "console.log(2);")

	// 切到临时 backend 目录里跑（后端用相对路径 ../web/dist 找前端）
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(backendDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	r := gin.New()
	setupFrontendStaticServing(r)

	// 1. 必须服务 ../web/dist 里**新的** index.html，而不是占位目录里的
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "fresh") {
		t.Errorf("expected the freshly built index.html from ../web/dist, got %q", truncate(w.Body.String()))
	}

	// 2. Service Worker 相关文件必须按原路径取到真实内容，而不是 index.html
	for path, want := range map[string]string{
		"/registerSW.js":        "export const x",
		"/sw.js":                "self.addEventListener",
		"/workbox-abc.js":       "console.log(2)",
		"/assets/app.js":        "console.log(1)",
		"/icons/icon-192.png":   "PNG",
		"/fonts/f.woff2":        "wOFF2",
		"/manifest.webmanifest": `{"name"`,
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: expected body containing %q, got %q", path, want, truncate(w.Body.String()))
		}
	}
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
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
