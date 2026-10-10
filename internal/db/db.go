package db

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// InitDB 初始化 SQLite 数据库
func InitDB(dbPath string) (*gorm.DB, error) {
	if dbPath == "" {
		dbPath = config.DefaultDBPath
	}

	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	// 启用 WAL 模式、5秒忙碌重试与普通同步模式，避免并发写锁死
	dsn := dbPath
	if !strings.Contains(dsn, "?") {
		dsn += "?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL"
	}

	database, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}

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
		&models.SiteConfig{},
		&models.Device{},
		&models.PairTicket{},
		&models.CuratorProfile{},
		&models.QRSession{},
		&models.Wish{},
		&models.Footprint{},
	)
	if err != nil {
		return nil, err
	}

	DB = database
	seedData(database)
	return database, nil
}

// seedData 初始化站点全局配置与主理人初始档案 (空态冷启动 RG-05)
func seedData(database *gorm.DB) {
	// 1. 站点全局与备案合规配置 (全新部署置未初始化 RG-01)
	var cfgCount int64
	database.Model(&models.SiteConfig{}).Count(&cfgCount)
	if cfgCount == 0 {
		config := models.SiteConfig{
			SiteName:        "散页 · 时光手账与生活档案",
			SiteSubtitle:    "时光手账与生活档案 · 记录真实可感的日子",
			CuratorName:     "迟暮",
			DefaultLocation: "中国 · 杭州",
			SiteDesc:        "生活档案与时光手账，记录真实可感的日子。",
			Domain:          "codeactivityhub.top",
			ICPNumber:       "浙ICP备2026081664号",
			ICPLink:         "https://beian.miit.gov.cn",
			Initialized:     false,
		}
		database.Create(&config)
	}

	// 2. 初始化主理人身份中枢 (Curator Profile)
	var profileCount int64
	database.Model(&models.CuratorProfile{}).Count(&profileCount)
	if profileCount == 0 {
		profile := models.CuratorProfile{
			Name:        "迟暮",
			Title:       "独立开发者 · 胶片摄影与手冲咖啡学徒",
			Bio:         "在数字洪流中筑造一座安静的纸质庇护所，记录具体而微的真实生活。",
			Location:    "中国 · 杭州",
			CurrentCity: "杭州",
			AvatarURL:   "",
			Motto:       "真实生活大于一切",
			Manifesto:   "不做功利的知识管理，不建庞杂的卡片盒。只记日落、快门光圈、一杯手冲浅烘、一次深夜长谈，与每一个平淡却不可复制的当下。",
			SocialLinks: `{"github":"https://github.com/YN1753","email":"curator@looseleaf.me"}`,
			EDCGear:     `[{"name":"Contax T2","category":"影像器材","spec":"Carl Zeiss Sonnar 38mm F2.8","note":"极简旁轴机械手感，锐利且通透的德系发色，记录四季街头。"},{"name":"MacBook Pro 14 (M3 Max)","category":"工作台算力","spec":"Space Black · 36GB Unified Memory","note":"随身代码与文字创作工坊，安静而深邃的算力中枢。"}]`,
		}
		database.Create(&profile)
	}

	log.Println("Database schema and core configurations ready.")
}
