package handlers

import (
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
)

// StorageConfigRequest 对象存储配置请求体
type StorageConfigRequest struct {
	AccountID            string `json:"account_id"`
	AccessKeyID          string `json:"access_key_id"`
	SecretAccessKey      string `json:"secret_access_key"`
	BucketName           string `json:"bucket_name"`
	Endpoint             string `json:"endpoint"`
	PublicDomain         string `json:"public_domain"`
	PresignExpireSeconds int    `json:"presign_expire_seconds"`
}

// LoadStorageConfigFromDB 系统启动后从数据库读取已持久化的对象存储配置并装载至运行时
func LoadStorageConfigFromDB() {
	var siteCfg models.SiteConfig
	if err := db.DB.First(&siteCfg).Error; err != nil {
		return
	}

	// 若主理人明确选择禁用对象存储（恢复本地模式）
	if siteCfg.R2Disabled {
		log.Println("Notice: Object storage is explicitly disabled in site configuration. Using local storage mode.")
		config.AppConfig.R2 = config.R2Config{}
		_ = storage.InitR2(config.AppConfig.R2)
		return
	}

	// 若数据库中存在有效的存储配置
	if strings.TrimSpace(siteCfg.R2BucketName) != "" && strings.TrimSpace(siteCfg.R2AccessKeyID) != "" {
		endpoint := strings.TrimSpace(siteCfg.R2Endpoint)
		accountID := strings.TrimSpace(siteCfg.R2AccountID)
		if endpoint == "" && accountID != "" {
			endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)
		}

		expireSec := siteCfg.R2PresignExpireSeconds
		if expireSec <= 0 {
			expireSec = config.AppConfig.R2.PresignExpireSeconds
		}
		if expireSec <= 0 {
			expireSec = 1800
		}

		cfg := config.R2Config{
			AccountID:            accountID,
			AccessKeyID:          strings.TrimSpace(siteCfg.R2AccessKeyID),
			SecretAccessKey:      strings.TrimSpace(siteCfg.R2SecretAccessKey),
			BucketName:           strings.TrimSpace(siteCfg.R2BucketName),
			Endpoint:             endpoint,
			PublicDomain:         strings.TrimRight(strings.TrimSpace(siteCfg.R2PublicDomain), "/"),
			PresignExpireSeconds: expireSec,
		}

		config.AppConfig.R2 = cfg
		if err := storage.InitR2(cfg); err != nil {
			log.Printf("Notice: DB R2 storage initialization: %v", err)
		} else if storage.IsConfigured() {
			log.Println("Cloudflare R2 storage initialized from database configuration.")
		}
	}
}

// normalizeR2AccountAndEndpoint 支持用户在 Account ID 框中直接粘贴完整 https://<account_id>.r2.cloudflarestorage.com 终结点
func normalizeR2AccountAndEndpoint(rawAccountID, rawEndpoint string) (string, string) {
	acc := strings.TrimSpace(rawAccountID)
	ep := strings.TrimRight(strings.TrimSpace(rawEndpoint), "/")

	if strings.HasPrefix(acc, "http://") || strings.HasPrefix(acc, "https://") {
		acc = strings.TrimRight(acc, "/")
		if ep == "" {
			ep = acc
		}
		trimmed := strings.TrimPrefix(strings.TrimPrefix(acc, "https://"), "http://")
		if idx := strings.Index(trimmed, ".r2.cloudflarestorage.com"); idx > 0 {
			acc = trimmed[:idx]
		}
	}
	if ep != "" {
		// 若用户粘贴的 S3 API 终结点末尾带了 /bucket_name（Cloudflare 桶设置页的 S3 API 会带桶名后缀），自动剥离路径只保留 origin
		if strings.HasPrefix(ep, "https://") || strings.HasPrefix(ep, "http://") {
			schemeEnd := strings.Index(ep, "://") + 3
			if slashIdx := strings.Index(ep[schemeEnd:], "/"); slashIdx > 0 {
				ep = ep[:schemeEnd+slashIdx]
			}
		}
	}
	if ep == "" && acc != "" {
		ep = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", acc)
	}
	return acc, ep
}

// TestStorageConfig 测试指定对象存储配置是否连通 (POST /api/v1/storage/test)
func TestStorageConfig(c *gin.Context) {
	var req StorageConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "参数格式错误: "+err.Error())
		return
	}

	accountID, endpoint := normalizeR2AccountAndEndpoint(req.AccountID, req.Endpoint)
	accessKeyID := strings.TrimSpace(req.AccessKeyID)
	secretAccessKey := strings.TrimSpace(req.SecretAccessKey)
	bucketName := strings.TrimSpace(req.BucketName)

	// 若未填写密钥但系统已有保存的密钥，则沿用已有密钥进行握手测试
	if secretAccessKey == "" {
		secretAccessKey = config.AppConfig.R2.SecretAccessKey
	}

	if bucketName == "" || accessKeyID == "" || secretAccessKey == "" || endpoint == "" {
		Error(c, http.StatusBadRequest, "请填写完整的存储桶名称、Endpoint (或 Account ID)、Access Key ID 及 Secret Access Key")
		return
	}

	testCfg := config.R2Config{
		AccountID:       accountID,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		BucketName:      bucketName,
		Endpoint:        endpoint,
	}

	latencyMs, err := storage.TestR2Connection(c.Request.Context(), testCfg)
	if err != nil {
		Error(c, http.StatusBadRequest, "对象存储连接测试失败: "+err.Error())
		return
	}

	Success(c, gin.H{
		"status":     "ok",
		"message":    fmt.Sprintf("对象存储握手测试成功，读写校验正常 (耗时 %dms)", latencyMs),
		"latency_ms": latencyMs,
	})
}

// UpdateStorageConfig 保存并热重载对象存储配置 (PUT /api/v1/storage/config)
func UpdateStorageConfig(c *gin.Context) {
	var req StorageConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "参数格式错误: "+err.Error())
		return
	}

	accountID, endpoint := normalizeR2AccountAndEndpoint(req.AccountID, req.Endpoint)
	accessKeyID := strings.TrimSpace(req.AccessKeyID)
	secretAccessKey := strings.TrimSpace(req.SecretAccessKey)
	bucketName := strings.TrimSpace(req.BucketName)
	publicDomain := strings.TrimRight(strings.TrimSpace(req.PublicDomain), "/")

	// 沿用旧密钥逻辑：若留空且已有密钥，保留原密钥
	if secretAccessKey == "" {
		secretAccessKey = config.AppConfig.R2.SecretAccessKey
	}

	if bucketName == "" || accessKeyID == "" || secretAccessKey == "" || endpoint == "" {
		Error(c, http.StatusBadRequest, "请填写完整的存储桶名称、Endpoint (或 Account ID)、Access Key ID 及 Secret Access Key")
		return
	}

	expireSec := req.PresignExpireSeconds
	if expireSec <= 0 {
		expireSec = config.AppConfig.R2.PresignExpireSeconds
	}
	if expireSec <= 0 {
		expireSec = 1800
	}

	newCfg := config.R2Config{
		AccountID:            accountID,
		AccessKeyID:          accessKeyID,
		SecretAccessKey:      secretAccessKey,
		BucketName:           bucketName,
		Endpoint:             endpoint,
		PublicDomain:         publicDomain,
		PresignExpireSeconds: expireSec,
	}

	// 保存前进行一次连通性校验，确保凭证正确无误
	if _, err := storage.TestR2Connection(c.Request.Context(), newCfg); err != nil {
		Error(c, http.StatusBadRequest, "对象存储连通性校验失败，未保存配置: "+err.Error())
		return
	}

	// 持久化落盘至 SQLite SiteConfig
	var siteCfg models.SiteConfig
	if err := db.DB.First(&siteCfg).Error; err != nil {
		siteCfg = models.SiteConfig{
			Initialized: true,
		}
		if err := db.DB.Create(&siteCfg).Error; err != nil {
			Error(c, http.StatusInternalServerError, "保存站点配置失败: "+err.Error())
			return
		}
	}

	siteCfg.R2Disabled = false
	siteCfg.R2AccountID = accountID
	siteCfg.R2AccessKeyID = accessKeyID
	siteCfg.R2SecretAccessKey = secretAccessKey
	siteCfg.R2BucketName = bucketName
	siteCfg.R2Endpoint = endpoint
	siteCfg.R2PublicDomain = publicDomain
	siteCfg.R2PresignExpireSeconds = expireSec

	if err := db.DB.Save(&siteCfg).Error; err != nil {
		Error(c, http.StatusInternalServerError, "持久化对象存储配置失败: "+err.Error())
		return
	}

	// 热更新全局运行时配置与 S3 客户端
	config.AppConfig.R2 = newCfg
	if err := storage.InitR2(newCfg); err != nil {
		Error(c, http.StatusInternalServerError, "重载存储客户端失败: "+err.Error())
		return
	}

	Success(c, storage.GetStorageStatus())
}

// ResetStorageConfig 恢复为本地磁盘存储，关闭对象存储联动 (DELETE /api/v1/storage/config)
func ResetStorageConfig(c *gin.Context) {
	var siteCfg models.SiteConfig
	if err := db.DB.First(&siteCfg).Error; err == nil {
		siteCfg.R2Disabled = true
		siteCfg.R2AccountID = ""
		siteCfg.R2AccessKeyID = ""
		siteCfg.R2SecretAccessKey = ""
		siteCfg.R2BucketName = ""
		siteCfg.R2Endpoint = ""
		siteCfg.R2PublicDomain = ""
		siteCfg.R2PresignExpireSeconds = 1800
		_ = db.DB.Save(&siteCfg)
	}

	config.AppConfig.R2 = config.R2Config{}
	_ = storage.InitR2(config.AppConfig.R2)

	Success(c, storage.GetStorageStatus())
}

// MigrateStorageMedia 执行本地与 R2 云端媒体文件的双向增量补齐同步 (POST /api/v1/storage/migrate)
func MigrateStorageMedia(c *gin.Context) {
	if !storage.IsConfigured() {
		Error(c, http.StatusBadRequest, "对象存储尚未配置就绪，无法执行媒体迁移")
		return
	}

	ctx := c.Request.Context()
	localDir := storage.GetLocalUploadDir()
	uploadedToR2 := 0
	downloadedToLocal := 0

	// 1. 扫描本地上传目录，将尚未存在于 R2 的本地媒体文件补充上传至 R2
	if fi, err := os.Stat(localDir); err == nil && fi.IsDir() {
		_ = filepath.Walk(localDir, func(p string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info == nil || info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(localDir, p)
			if err != nil {
				return nil
			}
			objectKey := strings.TrimPrefix(filepath.ToSlash(rel), "/")
			if objectKey == "" || strings.Contains(objectKey, "..") {
				return nil
			}
			if _, _, err := storage.VerifyObjectExists(objectKey); err == nil {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			ct := mime.TypeByExtension(filepath.Ext(p))
			if ct == "" {
				ct = "application/octet-stream"
			}
			uploadErr := storage.UploadObject(ctx, objectKey, f, info.Size(), ct)
			f.Close()
			if uploadErr == nil {
				uploadedToR2++
			}
			return nil
		})
	}

	// 2. 遍历数据库中的所有附件记录，将本地缺失但存在于 R2 的媒体文件下载回本地磁盘
	var attachments []models.Attachment
	db.DB.Find(&attachments)
	for _, att := range attachments {
		cleanKey := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(att.ObjectKey)), "/")
		if cleanKey == "" || strings.Contains(cleanKey, "..") {
			continue
		}
		localPath := filepath.Join(localDir, filepath.FromSlash(cleanKey))
		if fi, err := os.Stat(localPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
			continue
		}
		rc, _, err := storage.GetObject(ctx, cleanKey)
		if err != nil || rc == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			rc.Close()
			continue
		}
		out, err := os.Create(localPath)
		if err != nil {
			rc.Close()
			continue
		}
		_, copyErr := io.Copy(out, rc)
		out.Close()
		rc.Close()
		if copyErr == nil {
			downloadedToLocal++
		}
	}

	Success(c, gin.H{
		"uploaded_to_r2":      uploadedToR2,
		"downloaded_to_local": downloadedToLocal,
		"message":             fmt.Sprintf("双向媒体同步完成：已上传 %d 个文件至 R2，已拉取 %d 个文件至本地", uploadedToR2, downloadedToLocal),
	})
}
