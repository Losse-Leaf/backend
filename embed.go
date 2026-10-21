package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// 嵌入 Web 前端构建产物 (单二进制分发，docs/ecosystem.md §10.4)
//
//go:embed all:web/dist
var embeddedWebDist embed.FS

// setupFrontendStaticServing 配置前端静态文件托管与 SPA 路由：
// 优先级 1：本地磁盘目录 (../web/dist 或 web/dist，优先服务本地动态构建与热重载)
// 优先级 2：二进制嵌入文件系统 (//go:embed web/dist，单二进制极简开箱分发)
// 优先级 3：仅 API 模式提示
func setupFrontendStaticServing(r *gin.Engine) {
	// 1. 优先尝试本地磁盘目录 (开发环境无感知调试)
	//
	// MOB-P1-xx：判据必须是「**这个目录里有没有可用的 index.html**」，
	// 而不是「这个目录存不存在」。
	//
	// 原先只看目录是否存在 ⇒ backend/web/dist（仓库里只留了一个 .gitkeep 占位）
	// 永远命中，开发时 `cd backend && go run .` 服务的是**嵌入二进制里的旧副本**，
	// 而不是 ../web/dist 里刚 `npm run build` 出来的新版本 ——
	// 表现为「改了前端却完全没生效」，极难排查。
	// 现在按顺序挑第一个**真有 index.html** 的目录：
	//   ../web/dist（仓库根的 web/，开发时最可能）→ web/dist（单二进制目录）→ 嵌入副本
	var distDir string
	for _, candidate := range []string{"../web/dist", "web/dist"} {
		indexPath := filepath.Join(candidate, "index.html")
		if info, err := os.Stat(indexPath); err == nil && info.Size() > 0 {
			distDir = candidate
			break
		}
	}
	if distDir == "" {
		// 三个来源都没有可用的 index.html —— 退回嵌入副本（见下方）
		distDir = "web/dist"
	}

	if stat, err := os.Stat(distDir); err == nil && stat.IsDir() {
		indexPath := filepath.Join(distDir, "index.html")
		if info, err := os.Stat(indexPath); err == nil && info.Size() > 0 {
			assetsDir := filepath.Join(distDir, "assets")
			r.GET("/assets/*filepath", func(c *gin.Context) {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
				c.FileFromFS(c.Param("filepath"), http.Dir(assetsDir))
			})
			r.StaticFile("/favicon.ico", filepath.Join(distDir, "favicon.ico"))
			r.StaticFile("/favicon.svg", filepath.Join(distDir, "favicon.svg"))
			r.StaticFile("/loose-leaf-icon.svg", filepath.Join(distDir, "loose-leaf-icon.svg"))
			r.StaticFile("/robots.txt", filepath.Join(distDir, "robots.txt"))
			// .webmanifest 不在 Go 的 mime 表里 ⇒ StaticFile 会发 text/plain，
			// 浏览器因此拒绝解析（manifest 静默失效）。
			// 显式声明正确的 Content-Type。
			r.GET("/manifest.webmanifest", func(c *gin.Context) {
				c.Header("Content-Type", "application/manifest+json")
				c.File(filepath.Join(distDir, "manifest.webmanifest"))
			})

			// MOB-P1-xx：Service Worker 相关的文件**必须**能按原路径取到。
			//
			// 此前本地磁盘分支只白名单了上面那几个文件，于是
			// /registerSW.js、/sw.js、/workbox-xxxx.js 全部落进 NoRoute ⇒
			// 返回的是 index.html（HTML！�� ⇒ 浏览器报
			//   "SyntaxError: Unexpected token '<'"  ⇒ **Service Worker 注册失败**，
			// PWA 的离线能力（预缓存 App Shell + 接口 NetworkFirst）整个不生效。
			// 嵌入分支是整目录服务，所以只有「本地磁盘优先」这条路径会犯病 ——
			// 也就是开发机上的 `cd backend && go run .`。
			//
			// 修法：这些文件按原路径显式路由出去（workbox 的文件名带哈希，用前缀匹配）。
			// SW 文件同理：不能 immutable（内容会随构建变），否则浏览器会拿旧版 SW 卡住更新
			r.GET("/registerSW.js", func(c *gin.Context) {
				c.Header("Cache-Control", "no-cache")
				c.File(filepath.Join(distDir, "registerSW.js"))
			})
			r.GET("/sw.js", func(c *gin.Context) {
				c.Header("Cache-Control", "no-cache")
				c.File(filepath.Join(distDir, "sw.js"))
			})
			// workbox 的产物文件名形如 workbox-f641ca17.js —— 是**连字符**，
			// 不是子目录。所以按 /workbox-:file 匹配（Gin 的路由不支持路径中间通配）。
			r.GET("/workbox-:file", func(c *gin.Context) {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
				c.File(filepath.Join(distDir, "workbox-"+c.Param("file")))
			})
			// PWA 图标（apple-touch-icon / 192 / 512 / maskable）同理：
			// 少了它们，manifest 里声明的图标全是 404，安装到桌面会变成默认图标。
			r.GET("/icons/*filepath", func(c *gin.Context) {
				c.Header("Cache-Control", "public, max-age=3600, must-revalidate")
				c.File(filepath.Join(distDir, "icons", filepath.Base(c.Param("filepath"))))
			})
			// 字体目录（打包内的自托管字体）
			// 字体**不设** immutable：这些 URL 在修复前曾返回过 index.html（404 被 SPA 回退吞掉），
			// 一旦被浏览器按 immutable 永久缓存，改对之后仍会一直拿旧的 HTML 副本
			// （表现为字体解析失败 "OTS parsing error"）。
			// 现在用可校验的短缓存，改完刷新即可生效。
			r.GET("/fonts/*filepath", func(c *gin.Context) {
				c.Header("Cache-Control", "public, max-age=3600, must-revalidate")
				c.File(filepath.Join(distDir, "fonts", filepath.Base(c.Param("filepath"))))
			})

			// SPA 单页应用回退到 index.html
			r.NoRoute(func(c *gin.Context) {
				if strings.HasPrefix(c.Request.URL.Path, "/api/") {
					c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "API endpoint not found"})
					return
				}
				c.Header("Cache-Control", "no-cache")
				c.File(indexPath)
			})
			log.Printf("Serving frontend from local disk: %s", distDir)
			return
		}
	}

	// 2. 检查嵌入的单二进制文件系统 (//go:embed all:web/dist)
	subFS, err := fs.Sub(embeddedWebDist, "web/dist")
	if err == nil {
		if indexFile, err := subFS.Open("index.html"); err == nil {
			_ = indexFile.Close()
			httpFS := http.FS(subFS)
			fileServer := http.FileServer(httpFS)

			r.NoRoute(func(c *gin.Context) {
				reqPath := c.Request.URL.Path
				if strings.HasPrefix(reqPath, "/api/") {
					c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "API endpoint not found"})
					return
				}

				// 检查请求的静态资产文件是否存在于嵌入文件系统中
				cleanPath := strings.TrimPrefix(reqPath, "/")
				if cleanPath != "" {
					if f, err := subFS.Open(cleanPath); err == nil {
						_ = f.Close()
						if strings.HasPrefix(cleanPath, "assets/") {
							c.Header("Cache-Control", "public, max-age=31536000, immutable")
						}
						fileServer.ServeHTTP(c.Writer, c.Request)
						return
					}
				}

				// SPA 单页路由 fallback 到 index.html
				c.Header("Cache-Control", "no-cache")
				c.Header("Content-Type", "text/html; charset=utf-8")
				indexData, err := fs.ReadFile(subFS, "index.html")
				if err == nil {
					c.Data(http.StatusOK, "text/html; charset=utf-8", indexData)
					return
				}

				c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "Index not found in embedded filesystem"})
			})
			log.Println("Serving frontend from embedded binary filesystem (//go:embed web/dist)")
			return
		}
	}

	// 3. 兜底 API 服务运行提示
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "API endpoint not found"})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{
			"message": "Loose-Leaf API service is running. Web frontend dist not found, build it via 'cd web && npm run build'.",
		})
	})
}
