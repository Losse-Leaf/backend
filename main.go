package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"chimu-lab/internal/config"
	"chimu-lab/internal/db"
	"chimu-lab/internal/handlers"
	"chimu-lab/internal/middleware"
	"chimu-lab/internal/storage"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	// 1. 加载配置并初始化 R2 存储 (支持 .env 自动读取)
	cfg := config.LoadConfig()
	if err := storage.InitR2(cfg.R2); err != nil {
		log.Printf("Notice: R2 storage initialization: %v", err)
	} else if storage.IsConfigured() {
		log.Println("Cloudflare R2 storage initialized successfully.")
	} else {
		log.Println("Notice: Cloudflare R2 storage credentials not configured. Text entries & transactions will work normally.")
	}

	if cfg.AdminAPIKey != "" {
		log.Println("Admin write protection enabled (ADMIN_API_KEY configured).")
	} else {
		log.Println("Notice: ADMIN_API_KEY not set. Write endpoints are open (suitable for local dev). Set ADMIN_API_KEY in .env for production.")
	}

	// 2. 初始化 SQLite 数据库 (已启用 WAL 模式与连接池优化)
	_, err := db.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// 3. 生产模式或开发模式
	if os.Getenv("GIN_MODE") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.Default()

	// 4. CORS 跨域配置
	corsConfig := cors.DefaultConfig()
	corsConfig.AllowAllOrigins = true
	corsConfig.AllowHeaders = []string{"Origin", "Content-Length", "Content-Type", "Authorization", "X-Admin-Key", "X-Device-Token"}
	corsConfig.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	r.Use(cors.New(corsConfig))

	// 5. API v1 路由组 (标准结构化数据与持久化服务)
	v1 := r.Group("/api/v1")
	{
		// === 公开只读接口 ===
		v1.GET("/entries", handlers.GetLifeEntriesV1)
		v1.GET("/entries/:id", handlers.GetLifeEntryByID)
		v1.GET("/storage/status", handlers.GetStorageStatus)
		v1.GET("/transactions", handlers.GetTransactions)
		v1.GET("/transactions/summary", handlers.GetTransactionSummary)
		v1.GET("/projects", handlers.GetProjectsV1)
		v1.GET("/projects/:id", handlers.GetProjectByID)
		v1.GET("/stats", handlers.GetGlobalStats)

		// 站点与主理人身份中枢 (全端动态获取，严禁前端硬编码)
		v1.GET("/config", handlers.GetConfigV1)
		v1.GET("/profile", handlers.GetCuratorProfile)

		// 系统生命周期与初次装帧冷启动
		v1.GET("/system/status", handlers.GetSystemStatus)
		v1.POST("/system/bootstrap", handlers.BootstrapSystem)

		// 网页端反向扫码登录与口令验证
		v1.POST("/auth/qr/create", handlers.CreateQRSession)
		v1.GET("/auth/qr/poll/:session_id", handlers.PollQRSession)
		v1.POST("/auth/verify", handlers.VerifyPasscode)

		// 设备免密配对 (公开握手接口)
		v1.POST("/auth/pair/confirm", handlers.ConfirmPairing)

		// 统一文件临时签名接口 (公开只读，供各端获取动态有效访问 URL)
		v1.GET("/files/sign", handlers.SignFileURL)
		v1.POST("/files/sign-batch", handlers.SignFilesBatch)

		// === 管理员写权限受保护接口 ===
		write := v1.Group("")
		write.Use(middleware.AdminAuthRequired())
		{
			// 统一文件直传接口 (支持 multipart/form-data，返回 R2 相对路径与临时预签名 URL)
			write.POST("/files/upload", handlers.UnifiedUploadFile)

			// Life Entries (生活档案)
			write.POST("/entries", handlers.CreateLifeEntry)
			write.PUT("/entries/:id", handlers.UpdateLifeEntry)
			write.DELETE("/entries/:id", handlers.DeleteLifeEntry)

			// Uploads & Storage (R2 预签名直传与完成)
			write.POST("/uploads/presign", handlers.PresignUpload)
			write.POST("/uploads/complete", handlers.CompleteUpload)
			write.POST("/uploads/cleanup", handlers.CleanupUpload)

			// Transactions (记账独立体系)
			write.POST("/transactions", handlers.CreateTransaction)
			write.PUT("/transactions/:id", handlers.UpdateTransaction)
			write.DELETE("/transactions/:id", handlers.DeleteTransaction)

			// Projects (项目与造物)
			write.POST("/projects", handlers.CreateProject)
			write.PUT("/projects/:id", handlers.UpdateProject)
			write.DELETE("/projects/:id", handlers.DeleteProject)

			// 站点全局配置与主理人档案维护
			write.PUT("/config", handlers.UpdateConfig)
			write.PUT("/profile", handlers.UpdateCuratorProfile)

			// 手机 App 确认授权网页扫码登录
			write.POST("/auth/qr/authorize", handlers.AuthorizeQRSession)

			// Device Pairing & Management (受信任设备管理)
			write.POST("/auth/pair/init", handlers.InitPairing)
			write.GET("/auth/devices", handlers.GetTrustedDevices)
			write.DELETE("/auth/devices/:id", handlers.RevokeDevice)
		}

		// Health Check
		v1.GET("/health", handlers.HealthCheck)
	}

	// 7. 本地媒体静态托管 (未配 R2 时服务 data/uploads/ 下的文件 RC-44)
	uploadsDir := filepath.Join("data", "uploads")
	_ = os.MkdirAll(uploadsDir, 0755)
	r.Static("/media", uploadsDir)

	// 8. 前端静态文件托管（支持从根目录或 backend 目录下直接启动）
	distDir := "web/dist"
	if stat, err := os.Stat(distDir); err != nil || !stat.IsDir() {
		if statUp, errUp := os.Stat("../web/dist"); errUp == nil && statUp.IsDir() {
			distDir = "../web/dist"
		}
	}

	if stat, err := os.Stat(distDir); err == nil && stat.IsDir() {
		assetsDir := filepath.Join(distDir, "assets")
		r.GET("/assets/*filepath", func(c *gin.Context) {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
			c.FileFromFS(c.Param("filepath"), http.Dir(assetsDir))
		})
		r.StaticFile("/favicon.ico", filepath.Join(distDir, "favicon.ico"))
		r.StaticFile("/favicon.svg", filepath.Join(distDir, "favicon.svg"))
		r.StaticFile("/icons.svg", filepath.Join(distDir, "icons.svg"))
		r.StaticFile("/robots.txt", filepath.Join(distDir, "robots.txt"))

		// SPA 单页应用回退到 index.html (带 no-cache 响应头，确保前端发版即时生效 BE-01)
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "API endpoint not found"})
				return
			}
			c.Header("Cache-Control", "no-cache")
			c.File(filepath.Join(distDir, "index.html"))
		})
		log.Printf("Serving frontend from %s", distDir)
	} else {
		r.NoRoute(func(c *gin.Context) {
			c.JSON(http.StatusNotFound, gin.H{
				"message": "Loose-Leaf API service is running. Web frontend dist not found, build it via 'cd web && npm run build'.",
			})
		})
	}

	port := cfg.Port
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 启动 HTTP 服务协程
	go func() {
		log.Printf("Loose-Leaf Server starting on :%s ...", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	// 8. 优雅停机 (Graceful Shutdown)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down Loose-Leaf server gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Loose-Leaf server exited cleanly.")
}
