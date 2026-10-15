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
	distDir := "web/dist"
	if stat, err := os.Stat(distDir); err != nil || !stat.IsDir() {
		if statUp, errUp := os.Stat("../web/dist"); errUp == nil && statUp.IsDir() {
			distDir = "../web/dist"
		}
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
			r.StaticFile("/manifest.webmanifest", filepath.Join(distDir, "manifest.webmanifest"))

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
