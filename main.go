package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"loose-leaf/internal/authclaim"
	"loose-leaf/internal/backup"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/handlers"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
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

	log.Println("Trusted device group authentication active.")

	// 2. 初始化 SQLite 数据库 (已启用 WAL 模式与连接池优化)
	_, err := db.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// --reopen-claim：所有设备全部丢失时的**本机自救通道**（重开首次认领入口）。
	// 安全边界：该标志只存在于进程启动参数中，不开放任何网络入口 ——
	// 能执行它的人必然已经物理登录服务器、可直接读写数据库文件，
	// 因此「重开认领入口」不会扩大任何远程攻击面；远程攻击者依然碰不到
	// 只打印在服务器标准输出上的一次性认领码。
	// 认领入口重开后完全复用已验证安全的一次性认领码链路：
	// 15 分钟 TTL、连续 5 次失败自动轮换（见 internal/authclaim）。
	if hasStartupFlag("--reopen-claim") {
		reopenClaimEntry()
	}

	// 全新实例启动时生成一次性认领码，只打印到标准输出，不落库、不写配置文件。
	// 认领码 15 分钟内有效；进程重启会重新生成。
	var siteCfg models.SiteConfig
	if err := db.DB.Limit(1).Find(&siteCfg).Error; err != nil {
		log.Printf("读取系统初始化状态失败: %v", err)
	} else if !siteCfg.Initialized {
		code, _, err := handlers.StartClaimCode()
		if err != nil {
			log.Printf("生成首次认领码失败: %v", err)
		} else {
			fmt.Printf("首次使用请打开 http://localhost:%s/ 并输入认领码：%s（15 分钟内有效）\n", cfg.Port, authclaim.Format(code))
		}
	}

	// BE-5：启动回填不再阻塞监听，改到 HTTP server 起来之后后台执行。
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
		// 整库恢复与健康探针必须在生命周期准入之前注册：
		//   - 恢复自身要拿数据库写锁，若再经读锁准入会自锁；
		//   - 探针不访问数据库，不能因长恢复而超时。
		restore := v1.Group("")
		restore.Use(restoreAdmission())
		restore.POST("/system/backup/restore", handlers.RestoreBackup)

		v1.GET("/health", handlers.HealthCheck)

		v1.Use(lifecycleGuard())

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

		// 系统生命周期与首台设备认领 (BE-18, BE-19)
		v1.GET("/system/status", handlers.GetSystemStatus)
		v1.POST("/auth/claim", handlers.ClaimDevice)

		// 设备配对握手/轮询 (公开接口；需系统已完成首次认领)
		v1.POST("/auth/pair/confirm", handlers.ConfirmPairing)
		v1.GET("/auth/pair/status", handlers.PollPairingStatus)
		v1.GET("/auth/pair/qr", handlers.RenderPairQRCode)
		// 会话探针与登出 (MOB-P1-69)：浏览器端用 HttpOnly Cookie 承载令牌后，
		// 需要一个不回传凭据的接口来判断「当前是不是主人」
		v1.GET("/auth/session", handlers.GetSessionStatus)
		v1.POST("/auth/logout", handlers.LogoutSession)

		// 统一文件临时签名接口 (公开只读，供各端获取动态有效访问 URL)
		v1.GET("/files/sign", handlers.SignFileURL)
		// 附件缩略图：列表与详情用它（几十 KB），灯箱才用原图
		v1.GET("/files/thumbnail", handlers.ThumbnailFile)
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
			// 恢复被删除的随身设备（⌘Z 撤销用）：连同伴生散页一起恢复
			write.POST("/assets/gear/restore", handlers.RestoreGearAsset)

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

			// 对象存储在线配置与连通性测试 (热重载)
			write.POST("/storage/test", handlers.TestStorageConfig)
			write.PUT("/storage/config", handlers.UpdateStorageConfig)
			write.DELETE("/storage/config", handlers.ResetStorageConfig)
			write.POST("/storage/migrate", handlers.MigrateStorageMedia)
		}

	}

	// 探针路由 (对齐 docs/backend.md §5.1)
	r.GET("/api/health", handlers.HealthCheck)
	r.GET("/health", handlers.HealthCheck)

	// 7. 本地媒体静态托管
	//
	// 关键：目录必须与 `storage.GetLocalUploadDir()` **完全一致**且是绝对路径。
	// 「本地磁盘为主」策略下，附件 URL 会直接指向本路由（/media/<object_key>），
	// 客户端由此同机直读，不必绕道对象存储。
	// 早先这里写的是相对路径 `data/uploads`，与进程工作目录耦合，
	// 一旦以 systemd / 容器方式启动，静态路由与实际写入目录就会错位。
	uploadsDir, err := filepath.Abs(storage.GetLocalUploadDir())
	if err != nil {
		uploadsDir = filepath.Join("data", "uploads")
	}
	_ = os.MkdirAll(uploadsDir, 0700)
	// 关键：媒体路由**带签名校验**（原先是无鉴权的静态托管 ——
	// 任何知道中枢地址的人都能翻阅全部照片）。
	// 放行条件：URL 自带有效签名，或携带有效的设备令牌 / 会话 Cookie。
	// ServeLocalMedia 会查库鉴权，因此同样受数据库生命周期准入保护。
	r.GET("/media/*filepath", lifecycleGuard(), handlers.ServeLocalMedia)

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

	// BE-5：HTTP 已开始监听后再后台回填历史散页/账目（分批 + 幂等 + sync.Once）。
	handlers.StartFootprintBackfill()

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

// hasStartupFlag 判断启动参数中是否携带了指定标志（如 --reopen-claim）。
// 兼容 `--flag` 与 `--flag=value` 两种写法；标志仅来自本机命令行，不构成网络入口。
func hasStartupFlag(name string) bool {
	for _, arg := range os.Args[1:] {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

// reopenClaimEntry 把 site_configs.initialized 置回 false，重新开启首次认领入口。
//
// 这是「所有受信任设备全部丢失」时的自救通道：保留全部业务数据（无需删库），
// 置回后下方原有的启动逻辑会立即生成并打印新的一次性认领码，/auth/claim 入口
// 随之重新开放，主理人用新码认领一台新设备即可重新拿回系统控制权。
//
// 关键（有意设计）：本函数**只能**经由 --reopen-claim 启动标志触达 ——
// 即仅限能在服务器上执行启动命令的物理访问者使用，不经任何网络暴露；
// 若重开失败（读库/写库出错）直接终止启动，绝不带着「以为重开了、实际没有」
// 的假象继续运行。
func reopenClaimEntry() {
	var cfg models.SiteConfig
	if err := db.DB.Limit(1).Find(&cfg).Error; err != nil {
		log.Fatalf("--reopen-claim: 读取站点配置失败: %v", err)
	}
	if cfg.ID == 0 {
		log.Println("--reopen-claim: 站点尚未初始化，首次认领入口本就开放，无需重开。")
		return
	}
	if !cfg.Initialized {
		log.Println("--reopen-claim: 首次认领入口当前已处于开放状态，无需重开。")
		return
	}
	if err := db.DB.Model(&models.SiteConfig{}).
		Where("id = ?", cfg.ID).
		Update("initialized", false).Error; err != nil {
		log.Fatalf("--reopen-claim: 重开认领入口失败: %v", err)
	}
	log.Println("已重新开启首次认领入口（--reopen-claim，业务数据全部保留），请查看下方打印的新认领码。")
}

// lifecycleGuard 为每个访问数据库的请求持有生命周期读锁。
//
// 恢复进行中直接 503；若恢复已开始但尚未拿到写锁（例如在等在途读写排空），
// 二次检查同样返回 503，避免请求挂在锁上直到 HTTP 超时。
func lifecycleGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if backup.IsRestoring() {
			abortRestoring(c)
			return
		}
		db.LockRead()
		defer db.UnlockRead()
		if backup.IsRestoring() {
			abortRestoring(c)
			return
		}
		c.Next()
	}
}

// restoreAdmission 整库恢复的专用准入：先 503 拦截并发恢复，再在短读锁内完成鉴权。
//
// 这条路由不能套 lifecycleGuard（否则恢复的写锁会与请求自身的读锁自锁），
// 但鉴权要查 devices 表，仍必须避开数据库文件替换窗口。
func restoreAdmission() gin.HandlerFunc {
	return func(c *gin.Context) {
		if backup.IsRestoring() {
			abortRestoring(c)
			return
		}
		db.LockRead()
		if backup.IsRestoring() {
			db.UnlockRead()
			abortRestoring(c)
			return
		}
		authorized := middleware.IsAdmin(c)
		db.UnlockRead()
		if !authorized {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    http.StatusUnauthorized,
				"message": "未授权的操作：缺少有效的访问凭证",
				"data":    nil,
			})
			return
		}
		c.Next()
	}
}

func abortRestoring(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"code":    http.StatusServiceUnavailable,
		"message": "系统正在从备份恢复，读写操作暂时不可用，请稍后重试",
		"data":    nil,
	})
}
