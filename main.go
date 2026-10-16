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

	"loose-leaf/internal/backup"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/handlers"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/storage"

	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
)

// Version 应用程序版本号，在构建时通过 -ldflags 注入 (RC-66)
var Version = "dev"

func main() {
	if Version != "dev" && config.Version == "dev" {
		config.Version = Version
	}

	// 1. 加载配置并初始化 R2 存储 (支持 .env 自动读取)
	cfg := config.LoadConfig()
	log.Printf("Starting Loose-Leaf core engine (version: %s)...", config.Version)
	if err := storage.InitR2(cfg.R2); err != nil {
		log.Printf("Notice: R2 storage initialization: %v", err)
	} else if storage.IsConfigured() {
		log.Println("Cloudflare R2 storage initialized successfully.")
	} else {
		log.Println("Notice: Cloudflare R2 storage credentials not configured. Text entries & transactions will work normally.")
	}

	log.Println("SQLite Master Passcode & Trusted Device Group authentication active.")

	// 2. 初始化 SQLite 数据库 (已启用 WAL 模式与连接池优化)
	_, err := db.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	handlers.EnsureEntryFootprintsSynced()
	handlers.LoadStorageConfigFromDB()

	// 3. 生产模式或开发模式
	if os.Getenv("GIN_MODE") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	var r *gin.Engine
	if gin.Mode() == gin.ReleaseMode {
		r = gin.New()
		r.Use(gin.Recovery())
	} else {
		r = gin.Default()
	}

	// 4. CORS 跨域配置 (支持环境变量 CORS_ALLOWED_ORIGINS 精确限制来源，默认允许跨端连接)
	corsConfig := cors.DefaultConfig()
	if allowedOrigins := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS")); allowedOrigins != "" {
		corsConfig.AllowOrigins = strings.Split(allowedOrigins, ",")
	} else {
		corsConfig.AllowAllOrigins = true
	}
	corsConfig.AllowHeaders = []string{"Origin", "Content-Length", "Content-Type", "Authorization", "X-Admin-Key", "X-Device-Token"}
	corsConfig.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	r.Use(cors.New(corsConfig))

	// 5. Gzip 压缩支持 (BE-01: 压缩静态资源与响应体，显著降低传输体积)
	r.Use(gzip.Gzip(gzip.DefaultCompression))

	// 5. API v1 路由组 (标准结构化数据与持久化服务)
	v1 := r.Group("/api/v1")
	{
		// === 公开只读接口 ===
		v1.GET("/entries", handlers.GetLifeEntriesV1)
		v1.GET("/entries/on-this-day", handlers.GetOnThisDayEntries)
		v1.GET("/entries/:id", handlers.GetLifeEntryByID)
		v1.GET("/search", handlers.GlobalSearch)
		v1.GET("/storage/status", handlers.GetStorageStatus)
		v1.GET("/projects", handlers.GetProjectsV1)
		v1.GET("/projects/:id", handlers.GetProjectByID)
		v1.GET("/stats", handlers.GetGlobalStats)

		// 站点与主理人身份中枢 (全端动态获取，严禁前端硬编码)
		v1.GET("/config", handlers.GetConfigV1)
		v1.GET("/profile", handlers.GetCuratorProfile)

		// 系统生命周期与初次装帧冷启动 (BE-18, BE-19)
		v1.GET("/system/status", handlers.GetSystemStatus)
		v1.POST("/system/bootstrap", handlers.BootstrapSystem)

		// 欢迎扉页管理员密钥登录与设备配对握手/轮询 (公开接口 BE-18, BE-19)
		v1.POST("/auth/verify", handlers.VerifyPasscode)
		v1.POST("/auth/pair/confirm", handlers.ConfirmPairing)
		v1.GET("/auth/pair/status", handlers.PollPairingStatus)
		v1.GET("/auth/pair/qr", handlers.RenderPairQRCode)

		// 统一文件临时签名接口 (公开只读，供各端获取动态有效访问 URL)
		v1.GET("/files/sign", handlers.SignFileURL)
		v1.POST("/files/sign-batch", handlers.SignFilesBatch)

		// 足迹图谱与回忆抽屉 (公开只读，RC-91)
		v1.GET("/footprints", handlers.GetFootprints)
		v1.GET("/footprints/atlas", handlers.GetFootprintsAtlas)
		v1.GET("/footprints/provinces/:adcode/drawer", handlers.GetProvinceMemoryDrawer)
		v1.GET("/footprints/cities/:city_code/drawer", handlers.GetCityMemoryDrawer)
		v1.GET("/footprints/:id", handlers.GetFootprintByID)

		// 三维心愿单 (公开只读)
		v1.GET("/wishes", handlers.GetWishes)
		v1.GET("/wishes/:id", handlers.GetWishByID)

		// 年度生活年鉴 (公开只读)
		v1.GET("/almanac/summary", handlers.GetAnnualAlmanacSummary)

		// 全局自定义分类字典 (公开只读)
		v1.GET("/categories", handlers.GetCategories)

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

			// Transactions (财务记账独立体系 - 需管理员鉴权 RC-53)
			write.GET("/transactions", handlers.GetTransactions)
			write.GET("/transactions/summary", handlers.GetTransactionSummary)
			write.POST("/transactions", handlers.CreateTransaction)
			write.PUT("/transactions/:id", handlers.UpdateTransaction)
			write.DELETE("/transactions/:id", handlers.DeleteTransaction)

			// 资产管理中枢与设备全生命周期 (RC-53, BE-15)
			write.GET("/assets/overview", handlers.GetAssetsOverview)
			write.PUT("/assets/liquid-balance", handlers.SetInitialLiquidBalance)
			write.POST("/assets/gear", handlers.AddGearAsset)
			write.POST("/assets/gear/:id/sell", handlers.SellGearAsset)
			write.DELETE("/assets/gear/:id", handlers.DeleteGearAsset)

			// 信贷分期渠道配置与独立还款日 (RC-53)
			write.GET("/credit-channels", handlers.GetCreditChannels)
			write.POST("/credit-channels", handlers.CreateCreditChannel)
			write.PUT("/credit-channels/:id", handlers.UpdateCreditChannel)
			write.DELETE("/credit-channels/:id", handlers.DeleteCreditChannel)

			// 用户自定义分类字典管理 (支出/收入/资产/支付方式/造物分类)
			write.POST("/categories", handlers.CreateCategory)
			write.PUT("/categories/:id", handlers.UpdateCategory)
			write.DELETE("/categories/:id", handlers.DeleteCategory)
			write.POST("/categories/reset", handlers.ResetCategories)

			// Projects (项目与造物)
			write.POST("/projects", handlers.CreateProject)
			write.PUT("/projects/:id", handlers.UpdateProject)
			write.DELETE("/projects/:id", handlers.DeleteProject)

			// Footprints (旅游足迹打卡)
			write.POST("/footprints", handlers.CreateFootprint)
			write.PUT("/footprints/:id", handlers.UpdateFootprint)
			write.DELETE("/footprints/:id", handlers.DeleteFootprint)

			// Wishes (三维心愿单与归册联动)
			write.POST("/wishes", handlers.CreateWish)
			write.PUT("/wishes/:id", handlers.UpdateWish)
			write.POST("/wishes/:id/resolve", handlers.ResolveWish)
			write.DELETE("/wishes/:id", handlers.DeleteWish)

			// 站点全局配置与主理人档案维护
			write.PUT("/config", handlers.UpdateConfig)
			write.PUT("/profile", handlers.UpdateCuratorProfile)

			// Device Pairing & Management (受信任设备管理)
			write.POST("/auth/pair/init", handlers.InitPairing)
			write.GET("/auth/devices", handlers.GetTrustedDevices)
			write.PUT("/auth/devices/:id", handlers.UpdateDeviceName)
			write.DELETE("/auth/devices/:id", handlers.RevokeDevice)

			// 主理人系统版本更新检查 (需管理员鉴权，访客零感知)
			write.GET("/system/update-check", handlers.CheckSystemUpdate)

			// 自动与手动全量备份 (ZIP 格式，最多保留 7 份)
			write.POST("/system/backup/now", handlers.CreateBackupNow)
			write.GET("/system/backup/list", handlers.GetBackupList)
			write.GET("/system/backup/download/:filename", handlers.DownloadBackup)
			write.DELETE("/system/backup/:filename", handlers.DeleteBackup)
			write.POST("/system/backup/restore", handlers.RestoreBackup)

			// 对象存储在线配置与连通性测试 (热重载)
			write.POST("/storage/test", handlers.TestStorageConfig)
			write.PUT("/storage/config", handlers.UpdateStorageConfig)
			write.DELETE("/storage/config", handlers.ResetStorageConfig)
			write.POST("/storage/migrate", handlers.MigrateStorageMedia)
		}

		// Health Check
		v1.GET("/health", handlers.HealthCheck)
	}

	// 探针路由 (对齐 docs/backend.md §5.1)
	r.GET("/api/health", handlers.HealthCheck)
	r.GET("/health", handlers.HealthCheck)

	// 7. 本地媒体静态托管 (未配 R2 时服务 data/uploads/ 下的文件 RC-44)
	uploadsDir := filepath.Join("data", "uploads")
	_ = os.MkdirAll(uploadsDir, 0755)
	r.Static("/media", uploadsDir)

	// 8. 前端静态文件托管（支持 //go:embed 单二进制内嵌与本地磁盘双模，docs/ecosystem.md §10.4）
	setupFrontendStaticServing(r)

	// 9. 启动每日自动定时备份任务 (北京时间 03:30)
	backup.StartDailyBackupScheduler()

	port := cfg.Port
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       120 * time.Second, // 支持 50MB 媒体文件在有限带宽下的平稳上传
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 启动 HTTP 服务协程
	go func() {
		log.Printf("Loose-Leaf Server %s starting on :%s ...", config.Version, port)
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
