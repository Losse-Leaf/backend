package db

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// lifecycleMu 保护 DB 连接池与磁盘文件的「代际」不被整库恢复打断。
//
// 约定：
//   - 任何会使用 DB 的请求或后台任务，在操作期间持读锁；
//   - 整库恢复持写锁，覆盖关闭旧池、替换文件、重连与解压媒体；
//   - 锁序固定为 lifecycle → backupMutex，禁止反向；
//   - 读锁不可重入：已持读锁的代码不得再调用 LockRead（写锁排队时会死锁）。
var lifecycleMu sync.RWMutex

// LockRead 获取数据库生命周期读锁
func LockRead() { lifecycleMu.RLock() }

// UnlockRead 释放数据库生命周期读锁
func UnlockRead() { lifecycleMu.RUnlock() }

// LockWrite 获取数据库生命周期写锁（仅整库恢复使用）
func LockWrite() { lifecycleMu.Lock() }

// UnlockWrite 释放数据库生命周期写锁
func UnlockWrite() { lifecycleMu.Unlock() }

// InitDB 初始化 SQLite 数据库
func InitDB(dbPath string) (*gorm.DB, error) {
	if dbPath == "" {
		dbPath = config.DefaultDBPath
	}

	dir := filepath.Dir(dbPath)
	// 关键：目录 0700 —— 数据库里存着 R2 桶的密钥与主理人口令哈希，
	// 0755 意味着**同机的其它用户可以读到**。
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// 已存在的目录也收紧一次（老部署可能是 0755）
	_ = os.Chmod(dir, 0700)

	// 启用 WAL 模式、5 秒忙碌重试与普通同步模式，避免并发写锁死。
	// 注意：glebarez/go-sqlite 只认 `_pragma` 查询参数，
	// 早期写的 `_journal_mode=WAL&_busy_timeout=...` 会被驱动**静默忽略** ——
	// 库实际一直跑在 rollback journal 模式下，与 WAL 相关的恢复/并发假设都不成立。
	dsn := dbPath
	if !strings.Contains(dsn, "?") {
		dsn += "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	}

	database, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:                                   logger.Default.LogMode(logger.Warn),
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, err
	}

	// 数据库文件本身也收紧到 0600（SQLite 驱动按 umask 落地，可能是 0644）。
	// 关键（发现 21）：必须在 gorm.Open（真正创建数据库文件）**之后**执行 ——
	// 早先放在 Open 之前，首次建库时文件尚不存在，chmod 空打，
	// 新库仍按 umask 落地，收紧从未对「首次创建」生效过。
	_ = os.Chmod(dbPath, 0600)

	// 调优连接池：SQLite 启用 WAL 模式后支持并发读取。配合 _busy_timeout=5000 适当放宽连接池并发度 (BE-07)
	if sqlDB, err := database.DB(); err == nil {
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
		sqlDB.SetConnMaxLifetime(time.Hour)
	}

	// 自动迁移表结构
	err = database.AutoMigrate(
		&models.LifeEntry{},
		&models.Attachment{},
		&models.Project{},
		&models.Transaction{},
		&models.CreditChannel{},
		&models.CategoryOption{},
		&models.SiteConfig{},
		&models.Device{},
		&models.PairTicket{},
		&models.CuratorProfile{},
		&models.Wish{},
		&models.Footprint{},
	)
	if err != nil {
		return nil, err
	}

	DB = database
	_ = database.Exec("DROP INDEX IF EXISTS idx_pair_tickets_pin_code;").Error
	_ = database.Exec("CREATE INDEX IF NOT EXISTS idx_att_refs ON attachments(entry_id, wish_id, footprint_id);").Error
	_ = database.Exec("CREATE INDEX IF NOT EXISTS idx_tx_type_occurred ON transactions(type, occurred_at DESC);").Error
	_ = database.Exec("CREATE INDEX IF NOT EXISTS idx_wish_status_kind_created ON wishes(status, kind, created_at DESC);").Error
	_ = database.Exec("CREATE INDEX IF NOT EXISTS idx_fp_city_visited ON footprints(city_code, visited_at DESC);").Error
	// 器物伴生散页曾使用契约外的 type="gear"，任何分类视角都看不到它。
	// 统一归入 life（gear_id 仍保留器物关联），并推进 updated_at，
	// 否则已缓存旧 type 的客户端走增量同步永远收不到这次修正。
	_ = database.Exec("UPDATE life_entries SET type = 'life', updated_at = ? WHERE type = 'gear'", config.NowInBeijing()).Error
	seedData(database)
	return database, nil
}

// DefaultCategoriesMap 系统预置的五大分类字典默认值
var DefaultCategoriesMap = map[string][]string{
	"expense": {"餐饮", "交通", "购物", "数码器材", "娱乐", "学习", "住房", "医疗", "旅行度假", "其他"},
	"income":  {"工资收入", "二手回血", "稿酬设计", "投资理财", "礼金红包", "退款报销", "其他收入"},
	"gear":    {"数码器材", "影像与暗房", "工作台与算力", "手冲咖啡工坊", "夜骑巡航", "纸笔与日常", "生活好物"},
	"payment": {"微信支付", "支付宝", "银行卡", "现金", "其他"},
	"project": {"开源项目", "独立开发", "硬件改造", "设计作品", "写作出版"},
}

// SeedDefaultCategories 为空态或指定 scope 补充预置分类字典
func SeedDefaultCategories(database *gorm.DB, targetScope string, forceRestore bool) {
	scopes := []string{"expense", "income", "gear", "payment", "project"}
	for _, sc := range scopes {
		if targetScope != "" && targetScope != sc {
			continue
		}
		defaults := DefaultCategoriesMap[sc]
		if !forceRestore {
			var cnt int64
			database.Model(&models.CategoryOption{}).Where("scope = ?", sc).Count(&cnt)
			if cnt > 0 {
				continue
			}
			batch := make([]models.CategoryOption, 0, len(defaults))
			for idx, name := range defaults {
				batch = append(batch, models.CategoryOption{
					Scope:     sc,
					Name:      name,
					SortOrder: (idx + 1) * 10,
					IsBuiltin: true,
				})
			}
			if len(batch) > 0 {
				database.Create(&batch)
			}
			continue
		}
		for idx, name := range defaults {
			var existing models.CategoryOption
			if err := database.Where("scope = ? AND name = ?", sc, name).Limit(1).Find(&existing).Error; err == nil && existing.ID == 0 {
				database.Create(&models.CategoryOption{
					Scope:     sc,
					Name:      name,
					SortOrder: (idx + 1) * 10,
					IsBuiltin: true,
				})
			}
		}
	}
}

// seedData 初始化站点全局配置与主理人初始档案 (空态冷启动 RG-05, RC-50, RC-52, RC-53)
func seedData(database *gorm.DB) {
	// 1. 站点全局配置 (全新部署置未初始化与纯净空态 RG-01, RC-50, RC-52)
	var cfgCount int64
	database.Model(&models.SiteConfig{}).Count(&cfgCount)
	if cfgCount == 0 {
		config := models.SiteConfig{
			SiteName:        "散页 · 个人生活志",
			SiteSubtitle:    "日常 · 资产 · 心愿 · 行迹 · 造物",
			CuratorName:     "",
			DefaultLocation: "",
			SiteDesc:        "记录日常、打理资产、兑现心愿、丈量山河、实践造物。",
			Initialized:     false,
		}
		database.Create(&config)
	} else {
		// 升级已有配置中的旧版俗套口号 (时光手账与生活档案 -> 散页 · 个人生活志)
		var existingCfg models.SiteConfig
		if err := database.First(&existingCfg).Error; err == nil {
			changed := false
			if strings.Contains(existingCfg.SiteName, "时光手账") || strings.Contains(existingCfg.SiteName, "生活档案与时光手账") {
				existingCfg.SiteName = "散页 · 个人生活志"
				changed = true
			}
			if strings.Contains(existingCfg.SiteSubtitle, "时光手账") {
				existingCfg.SiteSubtitle = "日常 · 资产 · 心愿 · 行迹 · 造物"
				changed = true
			}
			if strings.Contains(existingCfg.SiteDesc, "时光手账") {
				existingCfg.SiteDesc = "记录日常、打理资产、兑现心愿、丈量山河、实践造物。"
				changed = true
			}
			if changed {
				database.Save(&existingCfg)
			}
		}
	}

	// 2. 初始化主理人身份中枢 (Curator Profile 纯净空态 RC-52)
	var profileCount int64
	database.Model(&models.CuratorProfile{}).Count(&profileCount)
	if profileCount == 0 {
		profile := models.CuratorProfile{
			Name:        "",
			Title:       "",
			Bio:         "",
			Location:    "",
			CurrentCity: "",
			AvatarURL:   "",
			Motto:       "",
			Manifesto:   "",
			SocialLinks: `{}`,
			EDCGear:     `[]`,
		}
		database.Create(&profile)
	}

	// 3. 初始化默认内置信贷分期渠道 (花呗 10 号、京东白条 20 号 RC-53)
	var channelCount int64
	database.Model(&models.CreditChannel{}).Count(&channelCount)
	if channelCount == 0 {
		channels := []models.CreditChannel{
			{
				Code:         "huabei",
				Name:         "花呗",
				RepaymentDay: 10,
				IsBuiltin:    true,
				Color:        "var(--accent-warm)",
			},
			{
				Code:         "baitiao",
				Name:         "京东白条",
				RepaymentDay: 20,
				IsBuiltin:    true,
				Color:        "var(--accent-vermilion)",
			},
		}
		for _, ch := range channels {
			database.Create(&ch)
		}
	}

	// 4. 初始化五大模块的默认分类字典
	SeedDefaultCategories(database, "", false)

	log.Println("Database schema and core configurations ready.")
}
