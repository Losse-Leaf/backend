package main

import (
	"os"
	"path/filepath"
	"testing"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// --reopen-claim 自救通道的回归测试：
// 1. hasStartupFlag 只认本机启动参数里的标志（含 --flag=value 形式）；
// 2. reopenClaimEntry 把 initialized 置回 false，重复执行幂等。

func TestHasStartupFlag(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"looseleaf", "--port", "9000", "--reopen-claim"}
	if !hasStartupFlag("--reopen-claim") {
		t.Fatal("启动参数包含 --reopen-claim 时应识别到")
	}
	if hasStartupFlag("--reopen-claims") {
		t.Fatal("前缀相同但名称不同的标志不得误判")
	}

	os.Args = []string{"looseleaf", "--reopen-claim=1"}
	if !hasStartupFlag("--reopen-claim") {
		t.Fatal("应兼容 --flag=value 形式")
	}

	os.Args = []string{"looseleaf", "--port", "9000"}
	if hasStartupFlag("--reopen-claim") {
		t.Fatal("未携带标志时不得误判")
	}
}

func TestReopenClaimEntry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reopen_claim.db")
	if _, err := db.InitDB(dbPath); err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}

	// InitDB 的 seedData 已建好未初始化的站点配置：置为已初始化模拟「认领完成」状态
	var cfg models.SiteConfig
	if err := db.DB.Limit(1).Find(&cfg).Error; err != nil || cfg.ID == 0 {
		t.Fatalf("读取站点配置失败: %v (id=%d)", err, cfg.ID)
	}
	if err := db.DB.Model(&models.SiteConfig{}).
		Where("id = ?", cfg.ID).
		Update("initialized", true).Error; err != nil {
		t.Fatalf("预置已初始化状态失败: %v", err)
	}

	reopenClaimEntry()
	db.DB.Limit(1).Find(&cfg)
	if cfg.Initialized {
		t.Fatal("reopenClaimEntry 应把 initialized 置回 false")
	}

	// 入口已开放时重复执行：幂等，不报错也不再改写
	reopenClaimEntry()
	db.DB.Limit(1).Find(&cfg)
	if cfg.Initialized {
		t.Fatal("重复执行 reopenClaimEntry 应保持 initialized=false")
	}
}
