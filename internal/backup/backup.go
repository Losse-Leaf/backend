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
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

const (
	MaxBackupRetention = 7
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
		// 若 VACUUM INTO 偶发失败，降级为直接读取当前 db 文件复制
		log.Printf("[Backup] VACUUM INTO 失败 (%v)，尝试文件级复制作为保底", err)
		if cpErr := copyFile(config.AppConfig.DBPath, tempDBPath); cpErr != nil {
			zipWriter.Close()
			tempZipFile.Close()
			return nil, fmt.Errorf("生成数据库快照失败: %w", cpErr)
		}
	}

	// 将快照写入 ZIP: database/looseleaf.db
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
			rc, _, err := storage.GetObject(ctx, cleanKey)
			if err != nil || rc == nil {
				continue
			}
			data, readErr := io.ReadAll(rc)
			rc.Close()
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

	// 6. 分流存储：优先上传已配置的 Cloudflare R2，否则落盘本地
	if storage.IsConfigured() {
		r2Key := "backups/" + filename
		if err := storage.UploadObject(ctx, r2Key, tempZipFile, zipSize, "application/zip"); err != nil {
			tempZipFile.Close()
			return nil, fmt.Errorf("上传备份到 R2 失败: %w", err)
		}
		tempZipFile.Close()
		item.StorageType = "r2"
		log.Printf("[Backup] 成功生成全量快照并推送至 R2: %s (大小: %d 字节)", r2Key, zipSize)
	} else {
		tempZipFile.Close()
		localDir := GetLocalBackupDir()
		if err := os.MkdirAll(localDir, 0755); err != nil {
			return nil, fmt.Errorf("创建本地备份目录失败: %w", err)
		}
		targetPath := filepath.Join(localDir, filename)
		if err := copyFile(tempZipPath, targetPath); err != nil {
			return nil, fmt.Errorf("落盘本地备份失败: %w", err)
		}
		item.StorageType = "local"
		log.Printf("[Backup] 成功生成全量快照并保存至本地: %s (大小: %d 字节)", targetPath, zipSize)
	}

	// 7. 执行滑动窗口清理：仅保留最近 MaxBackupRetention (7) 份备份
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

// RestoreFromZipStream 从 ZIP 输入流执行整库还原与灾备回退
func RestoreFromZipStream(zipReader *zip.Reader) error {
	backupMutex.Lock()
	defer backupMutex.Unlock()

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

	// 3. 终极自保措施：在覆盖既有数据库前，生成一份紧急快照副本 (pre-restore-emergency)
	currentDBPath := config.AppConfig.DBPath
	emergencyBackupPath := fmt.Sprintf("%s.pre-restore-emergency-%d", currentDBPath, time.Now().Unix())
	if err := copyFile(currentDBPath, emergencyBackupPath); err != nil {
		log.Printf("[Backup] 生成当前在用库紧急保底副本失败: %v (继续执行)", err)
	} else {
		log.Printf("[Backup] 已生成当前在用库紧急保底副本: %s", emergencyBackupPath)
	}

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
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
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

	log.Printf("[Backup] 恭喜！已成功从备份完整恢复数据库与 %d 项媒体资源", len(uploadFiles))
	return nil
}

// StartDailyBackupScheduler 启动每日自动定时备份任务 (北京时间 03:30)
func StartDailyBackupScheduler() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[Backup Scheduler] 定时任务异常崩溃捕获: %v", r)
			}
		}()

		for {
			now := time.Now().In(config.BeijingLocation)
			// 计算下一个 03:30 的时刻
			nextRun := time.Date(now.Year(), now.Month(), now.Day(), 3, 30, 0, 0, config.BeijingLocation)
			if now.After(nextRun) {
				nextRun = nextRun.Add(24 * time.Hour)
			}

			waitDuration := nextRun.Sub(now)
			log.Printf("[Backup Scheduler] 下次每日全量自动备份定于: %s (等待 %s)",
				nextRun.Format("2006-01-02 15:04:05"), waitDuration.Round(time.Minute))

			timer := time.NewTimer(waitDuration)
			<-timer.C

			log.Println("[Backup Scheduler] 每日夜间定时备份任务已唤醒，开始生成全量归档...")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			if item, err := CreateBackup(ctx); err != nil {
				log.Printf("[Backup Scheduler] 自动备份执行失败: %v", err)
			} else {
				log.Printf("[Backup Scheduler] 自动备份成功完成: %s (%s, %d 字节)",
					item.Filename, item.StorageType, item.Size)
			}
			cancel()
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

// 辅助工具：文件安全拷贝
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
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
