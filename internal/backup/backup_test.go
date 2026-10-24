package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// setupRestoreTestDB 建立隔离的临时库并返回一条基线记录。
func setupRestoreTestDB(t *testing.T) (string, models.LifeEntry) {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "looseleaf.db")
	previousPath := config.AppConfig.DBPath
	config.AppConfig.DBPath = dbPath
	t.Cleanup(func() {
		if db.DB != nil {
			if sqlDB, err := db.DB.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
		config.AppConfig.DBPath = previousPath
	})
	database, err := db.InitDB(dbPath)
	if err != nil {
		t.Fatalf("初始化临时库失败: %v", err)
	}
	entry := models.LifeEntry{Type: "life", Title: "恢复前标题", Content: "恢复前正文", OccurredAt: time.Now()}
	if err := database.Create(&entry).Error; err != nil {
		t.Fatalf("写入基线记录失败: %v", err)
	}
	return dbPath, entry
}

// openTestRestoreZip 生成一份基线备份并打开其 ZIP 读取器。
func openTestRestoreZip(t *testing.T) *zip.ReadCloser {
	t.Helper()
	item, err := CreateBackup(context.Background())
	if err != nil {
		t.Fatalf("生成备份失败: %v", err)
	}
	zr, err := zip.OpenReader(filepath.Join(GetLocalBackupDir(), item.Filename))
	if err != nil {
		t.Fatalf("打开备份 ZIP 失败: %v", err)
	}
	t.Cleanup(func() { zr.Close() })
	return zr
}

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

func TestRestoreWaitsForInFlightReaders(t *testing.T) {
	_, entry := setupRestoreTestDB(t)
	zr := openTestRestoreZip(t)

	if err := db.DB.Model(&models.LifeEntry{}).Where("id = ?", entry.ID).Update("title", "恢复期间被篡改").Error; err != nil {
		t.Fatalf("篡改失败: %v", err)
	}

	db.LockRead()
	released := make(chan struct{})
	go func() {
		<-released
		db.UnlockRead()
	}()

	done := make(chan error, 1)
	go func() { done <- RestoreFromZipStream(&zr.Reader) }()

	deadline := time.Now().Add(3 * time.Second)
	for !IsRestoring() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !IsRestoring() {
		close(released)
		t.Fatal("恢复未进入隔离态")
	}

	select {
	case err := <-done:
		close(released)
		t.Fatalf("在途读请求未排空时恢复就已结束: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(released)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("恢复失败: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("读锁释放后恢复仍未完成")
	}

	if IsRestoring() {
		t.Error("恢复结束后必须清除隔离态")
	}
	var restored models.LifeEntry
	if err := db.DB.First(&restored, entry.ID).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if restored.Title != entry.Title {
		t.Fatalf("恢复后标题应为 %q，实际 %q", entry.Title, restored.Title)
	}
}

func TestRestoreEmergencySnapshotIncludesWAL(t *testing.T) {
	dbPath, _ := setupRestoreTestDB(t)
	zr := openTestRestoreZip(t)

	sqlDB, err := db.DB.DB()
	if err != nil {
		t.Fatalf("获取连接池失败: %v", err)
	}
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("获取独立连接失败: %v", err)
	}

	// 关闭该连接上的自动 checkpoint，让探针只存在于 WAL。
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatalf("关闭自动 checkpoint 失败: %v", err)
	}
	marker := "wal-only-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS wal_probe(value TEXT)"); err != nil {
		t.Fatalf("建探针表失败: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO wal_probe(value) VALUES (?)", marker); err != nil {
		t.Fatalf("写入探针失败: %v", err)
	}
	// 归还连接但不必等它成为「最后一个连接」才 checkpoint：
	// 池里还有其它空闲连接，因此关闭它不会把 WAL 刷进主库。
	if err := conn.Close(); err != nil {
		t.Fatalf("归还独立连接失败: %v", err)
	}

	walInfo, err := os.Stat(dbPath + "-wal")
	if err != nil || walInfo.Size() == 0 {
		t.Fatalf("WAL 未生效或为空: info=%v, err=%v", walInfo, err)
	}
	mainBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("读取主库文件失败: %v", err)
	}
	if bytes.Contains(mainBytes, []byte(marker)) {
		t.Fatal("探针已写入主库文件，测试前提（仅存于 WAL）不成立")
	}

	if err := RestoreFromZipStream(&zr.Reader); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	matches, err := filepath.Glob(dbPath + ".pre-restore-emergency-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("应保留且仅保留一份紧急快照，实际 %v (err=%v)", matches, err)
	}
	emergency, err := sql.Open("sqlite", matches[0])
	if err != nil {
		t.Fatalf("打开紧急快照失败: %v", err)
	}
	defer emergency.Close()

	var value string
	if err := emergency.QueryRow("SELECT value FROM wal_probe LIMIT 1").Scan(&value); err != nil {
		t.Fatalf("紧急快照丢失 WAL 中已提交的数据: %v", err)
	}
	if value != marker {
		t.Fatalf("紧急快照探针值 = %q，期望 %q", value, marker)
	}
	var integrity string
	if err := emergency.QueryRow("PRAGMA integrity_check;").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("紧急快照完整性校验失败: %q, %v", integrity, err)
	}
}

func TestRestoreAbortsWhenEmergencySnapshotFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录写权限限制，无法模拟快照失败")
	}
	dbPath, entry := setupRestoreTestDB(t)
	zr := openTestRestoreZip(t)

	if err := db.DB.Model(&models.LifeEntry{}).Where("id = ?", entry.ID).Update("title", "必须保留").Error; err != nil {
		t.Fatalf("篡改失败: %v", err)
	}
	dir := filepath.Dir(dbPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("无法收紧目录权限: %v", err)
	}
	defer os.Chmod(dir, 0o700)
	restoreErr := RestoreFromZipStream(&zr.Reader)
	_ = os.Chmod(dir, 0o700)

	if restoreErr == nil {
		t.Fatal("紧急快照失败时必须中止恢复")
	}
	if IsRestoring() {
		t.Error("中止后必须清除隔离态")
	}
	var kept models.LifeEntry
	if err := db.DB.First(&kept, entry.ID).Error; err != nil {
		t.Fatalf("原库应保持可用: %v", err)
	}
	if kept.Title != "必须保留" {
		t.Fatalf("紧急快照失败时不得覆盖原库，标题被改为 %q", kept.Title)
	}
	matches, _ := filepath.Glob(dbPath + ".pre-restore-emergency-*")
	if len(matches) != 0 {
		t.Fatalf("失败的快照不得留下半成品: %v", matches)
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
