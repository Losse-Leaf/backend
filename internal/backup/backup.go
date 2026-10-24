package backup

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

const (
	// MaxBackupRetention 保留的备份份数
	//
	// 备份频率改为每 6 小时后，保留 12 份 ≈ 3 天历史
	// （原先每日 1 次保留 7 份 ≈ 7 天；频率提高 4 倍，
	// 份数相应上调以维持相近的历史深度，磁盘占用仍在可接受范围）。
	MaxBackupRetention = 12
	BackupFilePrefix   = "looseleaf-backup-"
	BackupFileExt      = ".zip"
	LocalBackupSubDir  = "backups"
)

type BackupItem struct {
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	StorageType string    `json:"storage_type"` // "r2" or "local"
}

type Manifest struct {
	Version          string           `json:"version"`
	ExportedAt       time.Time        `json:"exported_at"`
	Generator        string           `json:"generator"`
	Counts           map[string]int64 `json:"counts"`
	ContainsDatabase bool             `json:"contains_database"`
	ContainsUploads  bool             `json:"contains_uploads"`
}

var (
	backupMutex sync.Mutex
)

// GetLocalBackupDir 返回本地备份存储目录路径 (默认为 data/backups)
func GetLocalBackupDir() string {
	base := filepath.Dir(config.AppConfig.DBPath)
	return filepath.Join(base, LocalBackupSubDir)
}

// GenerateBackupFilename 生成标准时间戳备份文件名
func GenerateBackupFilename() string {
	beijingTime := time.Now().In(config.BeijingLocation)
	return fmt.Sprintf("%s%s%s", BackupFilePrefix, beijingTime.Format("20060102-150405"), BackupFileExt)
}

// CreateBackup 执行一次完整的全量原子打包，生成 .zip 归档，并上传 R2 或落盘本地（滑动窗口保留最新 7 份）
func CreateBackup(ctx context.Context) (*BackupItem, error) {
	backupMutex.Lock()
	defer backupMutex.Unlock()

	filename := GenerateBackupFilename()
	tempZipFile, err := os.CreateTemp("", "looseleaf-backup-*.zip")
	if err != nil {
		return nil, fmt.Errorf("创建临时备份文件失败: %w", err)
	}
	tempZipPath := tempZipFile.Name()
	defer os.Remove(tempZipPath)

	zipWriter := zip.NewWriter(tempZipFile)

	// 1. 创建 SQLite 热快照 (VACUUM INTO)
	tempDBFile, err := os.CreateTemp("", "looseleaf-snapshot-*.db")
	if err != nil {
		zipWriter.Close()
		tempZipFile.Close()
		return nil, fmt.Errorf("创建快照临时文件失败: %w", err)
	}
	tempDBPath := tempDBFile.Name()
	tempDBFile.Close()
	os.Remove(tempDBPath) // VACUUM INTO 需要目标文件尚不存在
	defer os.Remove(tempDBPath)

	if err := db.DB.Exec("VACUUM INTO ?", tempDBPath).Error; err != nil {
		// 关键（发现 9）：VACUUM INTO 失败时**不再降级为「直接复制活库文件」**。
		// 主库运行在 WAL 模式下，直接复制主文件可能缺少尚未 checkpoint 的事务，
		// 产出**不一致**的副本 —— 一份看似成功、实则损坏的备份比没有备份更危险
		// （恢复时会用它覆盖掉唯一的好库）。宁可本轮失败，等待下一轮（≤6 小时）重试。
		zipWriter.Close()
		tempZipFile.Close()
		return nil, fmt.Errorf("生成数据库快照失败 (VACUUM INTO): %w", err)
	}

	// 关键：**打包前先对快照副本脱敏**。
	//
	// 原先把 VACUUM 出来的**原始数据库**直接打进 ZIP，
	// 而脱敏只作用于 metadata/data.json 那份镜像 ——
	// 于是 `site_configs.r2_secret_access_key`（**明文**列）与
	// 主理人口令哈希、以及全部私人内容都原样进了备份包。
	// 拿到 ZIP = 同时拿到「所有散页正文/照片/账目」+「R2 桶的写权限密钥」
	// +「可离线爆破的口令哈希」。
	//
	// 脱敏在**副本**上做，绝不碰线上库。
	if err := sanitizeSnapshotDB(tempDBPath); err != nil {
		// 脱敏失败宁可不打这一份，也不能带着密钥出去
		zipWriter.Close()
		tempZipFile.Close()
		return nil, fmt.Errorf("备份脱敏失败，已中止打包: %w", err)
	}

	// 将**已脱敏**的快照写入 ZIP: database/looseleaf.db
	if err := addFileToZip(zipWriter, tempDBPath, "database/looseleaf.db"); err != nil {
		zipWriter.Close()
		tempZipFile.Close()
		return nil, fmt.Errorf("打包数据库快照失败: %w", err)
	}

	// 2. 统计并收集元数据 JSON
	counts := make(map[string]int64)
	var entries []models.LifeEntry
	db.DB.Find(&entries)
	counts["entries"] = int64(len(entries))

	var wishes []models.Wish
	db.DB.Find(&wishes)
	counts["wishes"] = int64(len(wishes))

	var footprints []models.Footprint
	db.DB.Find(&footprints)
	counts["footprints"] = int64(len(footprints))

	var projects []models.Project
	db.DB.Find(&projects)
	counts["projects"] = int64(len(projects))

	var transactions []models.Transaction
	db.DB.Find(&transactions)
	counts["transactions"] = int64(len(transactions))

	var categories []models.CategoryOption
	db.DB.Find(&categories)
	counts["categories"] = int64(len(categories))

	var creditChannels []models.CreditChannel
	db.DB.Find(&creditChannels)
	counts["credit_channels"] = int64(len(creditChannels))

	var attachments []models.Attachment
	db.DB.Find(&attachments)
	counts["attachments"] = int64(len(attachments))

	var siteConfig models.SiteConfig
	db.DB.Limit(1).Find(&siteConfig)
	siteConfig.MasterPasscode = ""
	siteConfig.R2SecretAccessKey = ""

	var curatorProfile models.CuratorProfile
	db.DB.Limit(1).Find(&curatorProfile)

	// 3. 将全量数据镜像写入 ZIP: metadata/data.json
	fullDump := map[string]interface{}{
		"site_config":     siteConfig,
		"curator_profile": curatorProfile,
		"entries":         entries,
		"wishes":          wishes,
		"footprints":      footprints,
		"projects":        projects,
		"transactions":    transactions,
		"categories":      categories,
		"credit_channels": creditChannels,
		"attachments":     attachments,
	}
	if dumpBytes, err := json.MarshalIndent(fullDump, "", "  "); err == nil {
		_ = addBytesToZip(zipWriter, dumpBytes, "metadata/data.json")
	}

	// 4. 打包本地上传的媒体资源与云端 R2 媒体资源 (uploads/*)
	localUploadDir := storage.GetLocalUploadDir()
	hasUploads := false
	packedUploadKeys := make(map[string]bool)
	if fi, err := os.Stat(localUploadDir); err == nil && fi.IsDir() {
		_ = filepath.Walk(localUploadDir, func(p string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(localUploadDir, p)
			if err != nil {
				return nil
			}
			slashRel := filepath.ToSlash(rel)
			packedUploadKeys[slashRel] = true
			hasUploads = true
			_ = addFileToZip(zipWriter, p, filepath.ToSlash(filepath.Join("uploads", rel)))
			return nil
		})
	}
	if storage.IsConfigured() {
		for _, att := range attachments {
			cleanKey := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(att.ObjectKey)), "/")
			if cleanKey == "" || packedUploadKeys[cleanKey] {
				continue
			}
			// 打包媒体时与用户看到的保持一致：本地优先，本地没有才回落 R2
			data, readErr := storage.ReadObject(ctx, cleanKey)
			if readErr == nil {
				if err := addBytesToZip(zipWriter, data, filepath.ToSlash(filepath.Join("uploads", cleanKey))); err == nil {
					packedUploadKeys[cleanKey] = true
					hasUploads = true
				}
			}
		}
	}

	// 5. 写入 manifest.json
	manifest := Manifest{
		Version:          config.Version,
		ExportedAt:       time.Now().In(config.BeijingLocation),
		Generator:        "散页 · 个人生活志",
		Counts:           counts,
		ContainsDatabase: true,
		ContainsUploads:  hasUploads,
	}
	if manifestBytes, err := json.MarshalIndent(manifest, "", "  "); err == nil {
		_ = addBytesToZip(zipWriter, manifestBytes, "manifest.json")
	}

	if err := zipWriter.Close(); err != nil {
		tempZipFile.Close()
		return nil, fmt.Errorf("关闭压缩包写入流失败: %w", err)
	}

	zipStat, err := tempZipFile.Stat()
	if err != nil {
		tempZipFile.Close()
		return nil, fmt.Errorf("获取压缩包元数据失败: %w", err)
	}
	zipSize := zipStat.Size()
	_, _ = tempZipFile.Seek(0, io.SeekStart)

	item := &BackupItem{
		Filename:  filename,
		Size:      zipSize,
		CreatedAt: manifest.ExportedAt,
	}

	// 6. 本地始终留一份，R2 只做异地副本。
	// 原先是「配了 R2 就只写 R2」：那一次上传失败（或 R2 临时不可用）就
	// 连本地副本都没有，灾备形同虚设。
	localDir := GetLocalBackupDir()
	if err := os.MkdirAll(localDir, 0700); err != nil {
		tempZipFile.Close()
		return nil, fmt.Errorf("创建本地备份目录失败: %w", err)
	}
	targetPath := filepath.Join(localDir, filename)
	if err := copyFile(tempZipPath, targetPath); err != nil {
		tempZipFile.Close()
		return nil, fmt.Errorf("落盘本地备份失败: %w", err)
	}
	item.StorageType = "local"

	if storage.IsConfigured() {
		r2Key := "backups/" + filename
		if err := storage.BackupObjectToR2(ctx, r2Key, tempZipFile, zipSize, "application/zip"); err != nil {
			// R2 失败不影响本地灾备，但必须显式记下：本次只有本地副本。
			log.Printf("[Backup] 本地快照已生成，但推送 R2 失败（本轮仅本地副本）: %v", err)
		} else {
			item.StorageType = "r2"
			log.Printf("[Backup] 全量快照已同时保存至本地与 R2: %s (大小: %d 字节)", targetPath, zipSize)
		}
	} else {
		log.Printf("[Backup] 成功生成全量快照并保存至本地: %s (大小: %d 字节)", targetPath, zipSize)
	}
	tempZipFile.Close()

	// 7. 执行滑动窗口清理：仅保留最近 MaxBackupRetention (12) 份备份
	_ = PruneOldBackups(ctx, MaxBackupRetention)

	return item, nil
}

// ListBackups 列出当前对象存储与本地保留的所有备份项（按时间倒序）
func ListBackups(ctx context.Context) ([]BackupItem, error) {
	var items []BackupItem

	// 1. 查询 R2 中的备份
	if storage.IsConfigured() {
		s3Objects, err := storage.ListObjects(ctx, "backups/"+BackupFilePrefix)
		if err != nil {
			log.Printf("[Backup] 列出 R2 备份失败: %v", err)
		} else {
			for _, obj := range s3Objects {
				fn := strings.TrimPrefix(obj.Key, "backups/")
				if !strings.HasSuffix(fn, BackupFileExt) {
					continue
				}
				items = append(items, BackupItem{
					Filename:    fn,
					Size:        obj.Size,
					CreatedAt:   obj.LastModified.In(config.BeijingLocation),
					StorageType: "r2",
				})
			}
		}
	}

	// 2. 查询本地目录备份
	localDir := GetLocalBackupDir()
	if entries, err := os.ReadDir(localDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), BackupFilePrefix) || !strings.HasSuffix(e.Name(), BackupFileExt) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			items = append(items, BackupItem{
				Filename:    e.Name(),
				Size:        info.Size(),
				CreatedAt:   info.ModTime().In(config.BeijingLocation),
				StorageType: "local",
			})
		}
	}

	// 3. 去重与按时间倒序排列
	seen := make(map[string]bool)
	var deduped []BackupItem
	for _, it := range items {
		if seen[it.Filename] {
			continue
		}
		seen[it.Filename] = true
		deduped = append(deduped, it)
	}

	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].CreatedAt.After(deduped[j].CreatedAt)
	})

	return deduped, nil
}

// PruneOldBackups 轮转清理旧备份，始终保持最多保留 maxKeep (7) 份
func PruneOldBackups(ctx context.Context, maxKeep int) error {
	if maxKeep <= 0 {
		maxKeep = MaxBackupRetention
	}

	// 1. 清理 R2
	if storage.IsConfigured() {
		s3Objects, err := storage.ListObjects(ctx, "backups/"+BackupFilePrefix)
		if err == nil && len(s3Objects) > maxKeep {
			sort.Slice(s3Objects, func(i, j int) bool {
				return s3Objects[i].LastModified.After(s3Objects[j].LastModified)
			})
			for _, obj := range s3Objects[maxKeep:] {
				log.Printf("[Backup] 轮转清理 R2 超额过期备份: %s", obj.Key)
				_ = storage.DeleteObject(obj.Key)
			}
		}
	}

	// 2. 清理本地
	localDir := GetLocalBackupDir()
	if entries, err := os.ReadDir(localDir); err == nil {
		var localFiles []os.FileInfo
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), BackupFilePrefix) || !strings.HasSuffix(e.Name(), BackupFileExt) {
				continue
			}
			if info, err := e.Info(); err == nil {
				localFiles = append(localFiles, info)
			}
		}

		if len(localFiles) > maxKeep {
			sort.Slice(localFiles, func(i, j int) bool {
				return localFiles[i].ModTime().After(localFiles[j].ModTime())
			})
			for _, fi := range localFiles[maxKeep:] {
				toRemove := filepath.Join(localDir, fi.Name())
				log.Printf("[Backup] 轮转清理本地超额过期备份: %s", toRemove)
				_ = os.Remove(toRemove)
			}
		}
	}

	return nil
}

// DeleteBackup 删除指定备份文件（从 R2 及本地存储中删除）
func DeleteBackup(ctx context.Context, filename string) error {
	backupMutex.Lock()
	defer backupMutex.Unlock()

	filename = filepath.Base(strings.TrimSpace(filename))
	if !strings.HasPrefix(filename, BackupFilePrefix) || !strings.HasSuffix(filename, BackupFileExt) {
		return errors.New("非法备份文件名")
	}

	deleted := false

	// 1. 若 R2 已配置，尝试从 R2 删除
	if storage.IsConfigured() {
		r2Key := "backups/" + filename
		if err := storage.DeleteObject(r2Key); err == nil {
			deleted = true
		}
	}

	// 2. 尝试从本地目录删除
	localPath := filepath.Join(GetLocalBackupDir(), filename)
	if _, err := os.Stat(localPath); err == nil {
		if err := os.Remove(localPath); err == nil {
			deleted = true
		}
	}

	if !deleted {
		return errors.New("备份文件不存在或已清理")
	}

	return nil
}

// GetBackupStream 打开指定备份文件的读取流
func GetBackupStream(ctx context.Context, filename string) (io.ReadCloser, int64, error) {
	filename = filepath.Base(strings.TrimSpace(filename))
	if !strings.HasPrefix(filename, BackupFilePrefix) || !strings.HasSuffix(filename, BackupFileExt) {
		return nil, 0, errors.New("非法备份文件名")
	}

	// 优先在 R2 查找
	if storage.IsConfigured() {
		r2Key := "backups/" + filename
		reader, size, err := storage.GetObject(ctx, r2Key)
		if err == nil {
			return reader, size, nil
		}
	}

	// 本地查找
	localPath := filepath.Join(GetLocalBackupDir(), filename)
	fi, err := os.Stat(localPath)
	if err != nil {
		return nil, 0, errors.New("未找到该备份文件")
	}
	f, err := os.Open(localPath)
	if err != nil {
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// restoringCount 恢复进行中的请求数。
//
// 恢复期间连接池会被关闭重建、主库与媒体文件会被整体覆写 ——
// 在途读写要么报错、要么落到被替换前的旧文件上而**静默丢失**。
// 用计数而非布尔：并发提交的第二次恢复在等待写锁期间也必须让新请求继续 503，
// 不能被先完成的那次提前放行。
var restoringCount atomic.Int32

// IsRestoring 当前是否正在执行整库恢复（所有请求应被 503 拒绝）
func IsRestoring() bool { return restoringCount.Load() > 0 }

// RestoreFromZipStream 从 ZIP 输入流执行整库还原与灾备回退
func RestoreFromZipStream(zipReader *zip.Reader) error {
	// 1. 扫描 ZIP 校验核心文件
	var dbZipFile *zip.File
	var uploadFiles []*zip.File

	for _, f := range zipReader.File {
		cleanName := filepath.ToSlash(filepath.Clean(f.Name))
		if cleanName == "database/looseleaf.db" {
			dbZipFile = f
		} else if strings.HasPrefix(cleanName, "uploads/") && !f.FileInfo().IsDir() {
			uploadFiles = append(uploadFiles, f)
		}
	}

	if dbZipFile == nil {
		return errors.New("备份压缩包中缺少核心数据库文件 (database/looseleaf.db)，已中止恢复")
	}

	// 2. 解压数据库到临时文件并执行 SQLite 完整性检查 (Pre-flight check)
	tempExtractedDB, err := os.CreateTemp("", "looseleaf-restore-*.db")
	if err != nil {
		return fmt.Errorf("创建解压数据库临时文件失败: %w", err)
	}
	tempExtractedPath := tempExtractedDB.Name()
	defer os.Remove(tempExtractedPath)

	const maxExtractSize = 512 * 1024 * 1024 // 512MB 单文件解压安全上限，防范 Zip Bomb
	rc, err := dbZipFile.Open()
	if err != nil {
		tempExtractedDB.Close()
		return fmt.Errorf("读取压缩包中数据库失败: %w", err)
	}
	if _, err := io.Copy(tempExtractedDB, io.LimitReader(rc, maxExtractSize)); err != nil {
		rc.Close()
		tempExtractedDB.Close()
		return fmt.Errorf("写入临时数据库失败: %w", err)
	}
	rc.Close()
	tempExtractedDB.Close()

	// 校验解压出来的 SQLite 是否健康
	testDB, err := sql.Open("sqlite", tempExtractedPath)
	if err != nil {
		return fmt.Errorf("校验备份数据库失败: %w", err)
	}
	var checkResult string
	rowErr := testDB.QueryRow("PRAGMA integrity_check;").Scan(&checkResult)
	testDB.Close()
	if rowErr != nil || checkResult != "ok" {
		return fmt.Errorf("备份数据库完整性校验未通过 (%s)，已拒绝覆盖", checkResult)
	}

	// 校验通过后才进入隔离态：无效备份不应让全站短暂不可用。
	restoringCount.Add(1)
	defer restoringCount.Add(-1)

	// 写锁覆盖「排空在途读写 → 快照 → 替换 → 重连 → 媒体解压」全过程。
	// 锁序固定为 lifecycle → backupMutex；反向会与持读锁的手动备份互相等待。
	db.LockWrite()
	defer db.UnlockWrite()

	backupMutex.Lock()
	defer backupMutex.Unlock()

	if db.DB == nil {
		return errors.New("数据库尚未初始化，无法执行恢复")
	}

	// 3. 终极自保措施：在覆盖既有数据库前，生成一份紧急快照副本 (pre-restore-emergency)
	//
	// 关键：绝不能用 copyFile 直接复制主库文件 —— 主库运行在 WAL 模式，
	// 已提交但尚未 checkpoint 的事务只存在于 -wal 中，直接复制会得到**缺数据**的
	// 「保底副本」（恢复失败时它反而会覆盖掉唯一的好库）。
	// VACUUM INTO 读取的是一致性快照，会把 WAL 中的内容一并写入独立文件。
	currentDBPath := config.AppConfig.DBPath
	emergencyBackupPath := fmt.Sprintf("%s.pre-restore-emergency-%d", currentDBPath, time.Now().UnixNano())
	if err := db.DB.Exec("VACUUM INTO ?", emergencyBackupPath).Error; err != nil {
		_ = os.Remove(emergencyBackupPath)
		return fmt.Errorf("生成当前在用库紧急快照失败，已中止恢复: %w", err)
	}
	log.Printf("[Backup] 已生成当前在用库紧急保底副本: %s", emergencyBackupPath)
	// 关键：**只保留最近一份**。
	// 原先每执行一次「回滚还原」就在库旁多一个完整数据库副本
	// （含全部私人内容与 R2 密钥），代码里**没有任何地方删除**它们 ——
	// 10 次恢复就是 10 份全量档案堆积在磁盘上。
	pruneEmergencyCopies(currentDBPath, 1)

	// 4. 关闭旧连接池，清理残留 WAL / SHM 日志，覆写当前数据库文件
	if sqlDB, err := db.DB.DB(); err == nil {
		_ = sqlDB.Close()
	}
	_ = os.Remove(currentDBPath + "-wal")
	_ = os.Remove(currentDBPath + "-shm")

	// 替换数据库文件
	if err := copyFile(tempExtractedPath, currentDBPath); err != nil {
		// 尝试重新开启旧连接
		_, _ = db.InitDB(currentDBPath)
		return fmt.Errorf("覆写主数据库文件失败: %w", err)
	}

	// 5. 解压媒体资源到本地 uploads 目录
	localUploadDir := storage.GetLocalUploadDir()
	for _, uf := range uploadFiles {
		relPath := strings.TrimPrefix(filepath.ToSlash(uf.Name), "uploads/")
		cleanRel := filepath.Clean(relPath)
		if strings.HasPrefix(cleanRel, "..") {
			continue
		}
		targetPath := filepath.Join(localUploadDir, cleanRel)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
			_, _ = db.InitDB(currentDBPath)
			return fmt.Errorf("创建媒体恢复目录失败: %w", err)
		}
		urc, err := uf.Open()
		if err != nil {
			_, _ = db.InitDB(currentDBPath)
			return fmt.Errorf("读取备份媒体文件失败: %w", err)
		}
		out, err := os.Create(targetPath)
		if err != nil {
			urc.Close()
			_, _ = db.InitDB(currentDBPath)
			return fmt.Errorf("写入恢复媒体文件失败: %w", err)
		}
		if _, copyErr := io.Copy(out, io.LimitReader(urc, maxExtractSize)); copyErr != nil {
			out.Close()
			urc.Close()
			_, _ = db.InitDB(currentDBPath)
			return fmt.Errorf("解压媒体文件失败: %w", copyErr)
		}
		out.Close()
		urc.Close()

		if storage.IsConfigured() {
			if rf, err := os.Open(targetPath); err == nil {
				if stat, statErr := rf.Stat(); statErr == nil {
					_ = storage.UploadObject(context.Background(), filepath.ToSlash(cleanRel), rf, stat.Size(), "")
				}
				rf.Close()
			}
		}
	}

	// 6. 重新连接数据库
	if _, err := db.InitDB(currentDBPath); err != nil {
		return fmt.Errorf("重载数据库连接失败: %w", err)
	}

	// 7. 恢复后的存储配置保全（发现 4b）：
	// 脱敏备份里的 R2 配置必然为空；若照单全收，进程一重启
	// LoadStorageConfigFromDB 读到空配置 → 对象存储**静默失效**（回落纯本地）。
	// 若当前运行时已有可用 R2 配置且还原库未显式禁用（R2Disabled），把它回写进还原库。
	preserveRuntimeStorageConfig()

	log.Printf("[Backup] 恭喜！已成功从备份完整恢复数据库与 %d 项媒体资源", len(uploadFiles))
	return nil
}

// preserveRuntimeStorageConfig 恢复后把运行时的对象存储配置回写进还原库。
// 仅当：还原库未显式禁用对象存储（R2Disabled=false）、其 R2 配置为空、
// 且运行时确实持有完整配置时执行 —— 用户主动 Reset 的（R2Disabled=true）不动。
func preserveRuntimeStorageConfig() {
	if !storage.IsConfigured() {
		return
	}
	var siteCfg models.SiteConfig
	if err := db.DB.Limit(1).Find(&siteCfg).Error; err != nil || siteCfg.ID == 0 {
		return
	}
	if siteCfg.R2Disabled {
		return
	}
	if strings.TrimSpace(siteCfg.R2BucketName) != "" && strings.TrimSpace(siteCfg.R2AccessKeyID) != "" {
		return
	}
	cfg := storage.R2ConfigSnapshot()
	if strings.TrimSpace(cfg.BucketName) == "" || strings.TrimSpace(cfg.AccessKeyID) == "" {
		return
	}
	siteCfg.R2AccountID = cfg.AccountID
	siteCfg.R2AccessKeyID = cfg.AccessKeyID
	siteCfg.R2SecretAccessKey = cfg.SecretAccessKey
	siteCfg.R2BucketName = cfg.BucketName
	siteCfg.R2Endpoint = cfg.Endpoint
	siteCfg.R2PublicDomain = cfg.PublicDomain
	siteCfg.R2PresignExpireSeconds = cfg.PresignExpireSeconds
	if err := db.DB.Save(&siteCfg).Error; err != nil {
		log.Printf("[Backup] 回写运行时存储配置失败（重启后对象存储可能失效）: %v", err)
	} else {
		log.Println("[Backup] 已把运行时对象存储配置回写至还原库（防重启后静默失效）")
	}
}

// BackupInterval 自动全量备份的间隔
//
// 关键（DEC-05 口径 B）：原先是**每日一次**（03:30）——
// 于是 VPS 磁盘损坏时，最多**丢失一天**的记录。
// 现在改为**每 6 小时一次**，把灾难时的数据损失从「一天」压到「六小时」。
const BackupInterval = 6 * time.Hour

// StartDailyBackupScheduler 启动周期性自动全量备份任务
//
// 保留旧函数名以免影响调用方，但内部已是**每 6 小时**一轮（见 BackupInterval）。
func StartDailyBackupScheduler() {
	go func() {
		for {
			// 每一轮单独 recover：原先 defer 挂在 goroutine 外层，
			// 任意一次 CreateBackup panic 都会让整个自动备份永久停摆。
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[Backup Scheduler] 本轮定时任务异常崩溃捕获: %v", r)
					}
				}()

				now := time.Now().In(config.BeijingLocation)
				// 计算下一个整点后 6 小时刻度
				nextRun := now.Truncate(time.Hour).Add(BackupInterval)
				if !nextRun.After(now) {
					nextRun = nextRun.Add(BackupInterval)
				}

				waitDuration := nextRun.Sub(now)
				log.Printf("[Backup Scheduler] 下次全量自动备份定于: %s (等待 %s)",
					nextRun.Format("2006-01-02 15:04:05"), waitDuration.Round(time.Minute))

				timer := time.NewTimer(waitDuration)
				<-timer.C

				log.Println("[Backup Scheduler] 定时备份任务已唤醒，开始生成全量归档...")
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				if item, err := CreateBackup(ctx); err != nil {
					log.Printf("[Backup Scheduler] 自动备份执行失败: %v", err)
				} else {
					log.Printf("[Backup Scheduler] 自动备份成功完成: %s (%s, %d 字节)",
						item.Filename, item.StorageType, item.Size)
				}
			}()
		}
	}()
}

// 辅助工具：添加本地文件进 ZIP 压缩包
func addFileToZip(zw *zip.Writer, srcPath, zipPath string) error {
	file, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return err
	}

	header, err := zip.FileInfoHeader(stat)
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(zipPath)
	header.Method = zip.Deflate

	writer, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(writer, file)
	return err
}

// 辅助工具：添加内存字节切片进 ZIP 压缩包
func addBytesToZip(zw *zip.Writer, content []byte, zipPath string) error {
	header := &zip.FileHeader{
		Name:     filepath.ToSlash(zipPath),
		Method:   zip.Deflate,
		Modified: time.Now().In(config.BeijingLocation),
	}
	writer, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = writer.Write(content)
	return err
}

// pruneEmergencyCopies 只保留最近 N 份 pre-restore 紧急副本
func pruneEmergencyCopies(dbPath string, keeping int) {
	dir := filepath.Dir(dbPath)
	base := filepath.Base(dbPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	var copies []item
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, base+".pre-restore-emergency-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		copies = append(copies, item{path: filepath.Join(dir, name), mod: info.ModTime()})
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].mod.After(copies[j].mod) })
	for i, c := range copies {
		if i < keeping {
			continue
		}
		if err := os.Remove(c.path); err == nil {
			log.Printf("[Backup] 已清理过期的紧急保底副本: %s", filepath.Base(c.path))
		}
	}
}

// sanitizeSnapshotDB 清空备份快照里的凭据字段
//
// 关键：在**副本**上执行，线上库绝不能被改动。
// 需要清空的：
//   - site_configs.master_passcode（主理人口令哈希，可离线爆破）
//   - site_configs.r2_secret_access_key（R2 桶的写权限密钥，明文列）
//   - site_configs.r2_access_key_id（配合上面的密钥即可完全控制桶）
func sanitizeSnapshotDB(snapshotPath string) error {
	conn, err := sql.Open("sqlite", snapshotPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	stmts := []string{
		`UPDATE site_configs SET master_passcode = ''`,
		`UPDATE site_configs SET r2_secret_access_key = ''`,
		`UPDATE site_configs SET r2_access_key_id = ''`,
		`UPDATE site_configs SET r2_password = ''`,
	}
	for _, stmt := range stmts {
		// 表不存在 / 列不存在时忽略（不同版本列集合可能有差异）
		if _, err := conn.Exec(stmt); err != nil {
			log.Printf("[Backup] 脱敏语句执行失败（可忽略）: %v", err)
		}
	}
	return nil
}

// 辅助工具：文件安全拷贝
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
