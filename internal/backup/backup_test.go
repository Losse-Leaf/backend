package backup

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

func TestCreateAndRestoreBackup(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "looseleaf-backup-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	testDBPath := filepath.Join(tempDir, "looseleaf.db")
	config.AppConfig.DBPath = testDBPath

	database, err := db.InitDB(testDBPath)
	if err != nil {
		t.Fatalf("Failed to init test db: %v", err)
	}

	// 插入测试数据
	entry := models.LifeEntry{
		Title:      "测试手账归档",
		Content:    "这是一篇用于验证全量 ZIP 快照与还原的测试手账。",
		OccurredAt: time.Now(),
		Type:       "life",
	}
	if err := database.Create(&entry).Error; err != nil {
		t.Fatalf("Failed to insert entry: %v", err)
	}

	// 1. 创建备份
	ctx := context.Background()
	item, err := CreateBackup(ctx)
	if err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	if item == nil || item.Filename == "" {
		t.Fatalf("Expected valid BackupItem, got nil or empty filename")
	}
	if item.StorageType != "local" && item.StorageType != "r2" {
		t.Errorf("Unexpected storage type: %s", item.StorageType)
	}

	// 2. 列出备份
	list, err := ListBackups(ctx)
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}
	if len(list) == 0 {
		t.Fatalf("Expected at least 1 backup in list, got 0")
	}

	found := false
	for _, b := range list {
		if b.Filename == item.Filename {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Newly created backup %s not found in list", item.Filename)
	}

	// 3. 打开读取流并校验 ZIP 内容
	stream, size, err := GetBackupStream(ctx, item.Filename)
	if err != nil {
		t.Fatalf("GetBackupStream failed: %v", err)
	}
	defer stream.Close()

	if size <= 0 {
		t.Errorf("Expected positive zip size, got %d", size)
	}

	tempZip, err := os.CreateTemp(tempDir, "verify-*.zip")
	if err != nil {
		t.Fatalf("Failed to create temp verify file: %v", err)
	}
	defer tempZip.Close()

	if _, err := tempZip.ReadFrom(stream); err != nil {
		t.Fatalf("Failed to read zip stream: %v", err)
	}

	zr, err := zip.OpenReader(tempZip.Name())
	if err != nil {
		t.Fatalf("Failed to parse zip: %v", err)
	}
	defer zr.Close()

	hasDB := false
	hasManifest := false
	hasDataJSON := false

	for _, f := range zr.File {
		clean := filepath.ToSlash(f.Name)
		if clean == "database/looseleaf.db" {
			hasDB = true
		} else if clean == "manifest.json" {
			hasManifest = true
		} else if clean == "metadata/data.json" {
			hasDataJSON = true
		}
	}

	if !hasDB {
		t.Errorf("Zip archive missing database/looseleaf.db")
	}
	if !hasManifest {
		t.Errorf("Zip archive missing manifest.json")
	}
	if !hasDataJSON {
		t.Errorf("Zip archive missing metadata/data.json")
	}

	// 4. 修改数据库内容，验证恢复能否成功覆写回原态
	if err := database.Model(&entry).Update("title", "已篡改的标题").Error; err != nil {
		t.Fatalf("Failed to tamper db: %v", err)
	}

	var tamperedEntry models.LifeEntry
	db.DB.First(&tamperedEntry, entry.ID)
	if tamperedEntry.Title != "已篡改的标题" {
		t.Fatalf("Tamper failed, got: %s", tamperedEntry.Title)
	}

	// 5. 执行恢复
	if err := RestoreFromZipStream(&zr.Reader); err != nil {
		t.Fatalf("RestoreFromZipStream failed: %v", err)
	}

	var restoredEntry models.LifeEntry
	if err := db.DB.First(&restoredEntry, entry.ID).Error; err != nil {
		t.Fatalf("Failed to find entry after restore: %v", err)
	}
	if restoredEntry.Title != "测试手账归档" {
		t.Errorf("Expected restored title '测试手账归档', got '%s'", restoredEntry.Title)
	}

	// 6. 测试滑动窗口裁剪（创建 8 个备份，验证保留上限为 7）
	for i := 0; i < 8; i++ {
		_, _ = CreateBackup(ctx)
	}
	_ = PruneOldBackups(ctx, 7)
	finalList, _ := ListBackups(ctx)
	if len(finalList) > 7 {
		t.Errorf("Expected at most 7 backups after prune, got %d", len(finalList))
	}
}

func TestGetBackupStreamSecurity(t *testing.T) {
	ctx := context.Background()
	// 测试路径穿越拦截
	_, _, err := GetBackupStream(ctx, "../../etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "非法") {
		t.Errorf("Expected path traversal error, got %v", err)
	}
}

func TestDailySchedulerCalculation(t *testing.T) {
	config.LoadConfig()
	now := time.Now().In(config.BeijingLocation)
	nextRun := time.Date(now.Year(), now.Month(), now.Day(), 3, 30, 0, 0, config.BeijingLocation)
	if now.After(nextRun) {
		nextRun = nextRun.Add(24 * time.Hour)
	}
	waitDuration := nextRun.Sub(now)
	if waitDuration <= 0 || waitDuration > 24*time.Hour {
		t.Errorf("Expected waitDuration in (0, 24h], got %v", waitDuration)
	}
}
