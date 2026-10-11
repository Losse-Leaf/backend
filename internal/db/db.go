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
		&models.CreditChannel{},
		&models.SiteConfig{},
		&models.Device{},
		&models.PairTicket{},
		&models.CuratorProfile{},
		&models.Wish{},
		&models.Footprint{},
		&models.UpdateNotice{},
	)
	if err != nil {
		return nil, err
	}

	DB = database
	seedData(database)
	return database, nil
}

// seedData 初始化站点全局配置与主理人初始档案 (空态冷启动 RG-05, RC-50, RC-52, RC-53)
func seedData(database *gorm.DB) {
	// 1. 站点全局配置 (全新部署置未初始化与纯净空态 RG-01, RC-50, RC-52)
	var cfgCount int64
	database.Model(&models.SiteConfig{}).Count(&cfgCount)
	if cfgCount == 0 {
		config := models.SiteConfig{
			SiteName:        "散页 · 时光手账与生活档案",
			SiteSubtitle:    "时光手账与生活档案 · 记录真实可感的日子",
			CuratorName:     "",
			DefaultLocation: "",
			SiteDesc:        "生活档案与时光手账，记录真实可感的日子。",
			Initialized:     false,
		}
		database.Create(&config)
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

	log.Println("Database schema and core configurations ready.")
}
