package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"chimu-lab/internal/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3config "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

var (
	s3Client       *s3.Client
	presignClient  *s3.PresignClient
	r2Bucket       string
	r2PublicDomain string
	isConfigured   bool

	safeFilenameRegex = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
)

// InitR2 初始化 Cloudflare R2 / S3 客户端
func InitR2(cfg config.R2Config) error {
	if !cfg.IsConfigured() {
		isConfigured = false
		return nil
	}

	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:               cfg.Endpoint,
			HostnameImmutable: true,
			SigningRegion:     "auto",
		}, nil
	})

	awsCfg, err := s3config.LoadDefaultConfig(context.TODO(),
		s3config.WithEndpointResolverWithOptions(customResolver),
		s3config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
		s3config.WithRegion("auto"),
	)
	if err != nil {
		return fmt.Errorf("failed to load AWS config for R2: %w", err)
	}

	s3Client = s3.NewFromConfig(awsCfg)
	presignClient = s3.NewPresignClient(s3Client)
	r2Bucket = cfg.BucketName
	r2PublicDomain = cfg.PublicDomain
	isConfigured = true

	return nil
}

// IsConfigured 检查 R2 是否已配置就绪
func IsConfigured() bool {
	return isConfigured
}

// GetStorageStatus 获取存储服务当前公开状态 (不暴露密钥)
func GetStorageStatus() map[string]interface{} {
	cfg := config.AppConfig.R2
	expireSec := cfg.PresignExpireSeconds
	if expireSec <= 0 {
		expireSec = 1800
	}
	return map[string]interface{}{
		"provider":               "Cloudflare R2",
		"configured":             isConfigured,
		"bucket":                 cfg.BucketName,
		"public_domain":          cfg.PublicDomain,
		"endpoint":               cfg.Endpoint,
		"presign_expire_seconds": expireSec,
		"presign_expire_minutes": expireSec / 60,
	}
}

// GetPresignExpireDuration 获取预签名临时下载 URL 的配置有效时长 (默认 30 分钟 / 1800 秒)
func GetPresignExpireDuration() time.Duration {
	sec := config.AppConfig.R2.PresignExpireSeconds
	if sec <= 0 {
		sec = 1800
	}
	return time.Duration(sec) * time.Second
}

// sanitizeFilename 净化文件名，防止路径注入
func sanitizeFilename(raw string) string {
	base := filepath.Base(raw)
	ext := filepath.Ext(base)
	nameWithoutExt := strings.TrimSuffix(base, ext)

	safeName := safeFilenameRegex.ReplaceAllString(nameWithoutExt, "_")
	if len(safeName) > 50 {
		safeName = safeName[:50]
	}
	if safeName == "" {
		safeName = "file"
	}

	safeExt := strings.ToLower(safeFilenameRegex.ReplaceAllString(ext, ""))
	return safeName + safeExt
}

// GeneratePresignedPutURL 生成用于客户端直接上传到 R2 的预签名 PUT URL
func GeneratePresignedPutURL(filename, contentType string, size int64) (uploadID, objectKey, presignedURL string, expiresIn int, err error) {
	if !isConfigured {
		return "", "", "", 0, errors.New("R2 存储未配置，暂无法生成预签名上传凭证")
	}

	now := time.Now()
	cleanName := sanitizeFilename(filename)
	uid := uuid.New().String()

	// 设计规范：entries/{year}/{month}/{day}/{uuid}-{safe_filename}
	objectKey = fmt.Sprintf("entries/%04d/%02d/%02d/%s-%s",
		now.Year(), now.Month(), now.Day(), uid[:8], cleanName)

	expiresIn = 900 // 15 分钟
	expiresDuration := time.Duration(expiresIn) * time.Second

	putInput := &s3.PutObjectInput{
		Bucket:        aws.String(r2Bucket),
		Key:           aws.String(objectKey),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	}

	presignedReq, err := presignClient.PresignPutObject(context.TODO(), putInput, s3.WithPresignExpires(expiresDuration))
	if err != nil {
		return "", "", "", 0, fmt.Errorf("failed to presign PUT request: %w", err)
	}

	return uid, objectKey, presignedReq.URL, expiresIn, nil
}

// VerifyObjectExists 确认对象已实际上传到 R2，并获取真实大小与类型
func VerifyObjectExists(objectKey string) (size int64, contentType string, err error) {
	if !isConfigured {
		return 0, "", errors.New("R2 存储未配置")
	}

	headInput := &s3.HeadObjectInput{
		Bucket: aws.String(r2Bucket),
		Key:    aws.String(objectKey),
	}

	res, err := s3Client.HeadObject(context.TODO(), headInput)
	if err != nil {
		return 0, "", fmt.Errorf("object not found in R2: %w", err)
	}

	if res.ContentLength != nil {
		size = *res.ContentLength
	}
	if res.ContentType != nil {
		contentType = *res.ContentType
	}

	return size, contentType, nil
}

// DeleteObject 从 R2 删除指定对象
func DeleteObject(objectKey string) error {
	if !isConfigured || objectKey == "" {
		return nil
	}

	delInput := &s3.DeleteObjectInput{
		Bucket: aws.String(r2Bucket),
		Key:    aws.String(objectKey),
	}

	_, err := s3Client.DeleteObject(context.TODO(), delInput)
	return err
}

// GetPublicURL 计算公开访问 URL (用于静态无鉴权降级场景)
func GetPublicURL(objectKey string) string {
	if objectKey == "" {
		return ""
	}
	if strings.HasPrefix(objectKey, "http://") || strings.HasPrefix(objectKey, "https://") {
		return objectKey
	}
	if r2PublicDomain != "" {
		return fmt.Sprintf("%s/%s", strings.TrimRight(r2PublicDomain, "/"), path.Clean(objectKey))
	}
	if config.AppConfig.R2.Endpoint != "" && r2Bucket != "" {
		return fmt.Sprintf("%s/%s/%s", strings.TrimRight(config.AppConfig.R2.Endpoint, "/"), r2Bucket, path.Clean(objectKey))
	}
	return objectKey
}

// ExtractObjectKey 从全量 URL 或相对路径中提取出标准的对象存储相对路径 Key (如 "avatar/user.png")
func ExtractObjectKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// 已经是相对路径的情况 (不包含协议头)
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		clean := path.Clean(strings.Trim(raw, "/\\"))
		if clean == "." || clean == "/" {
			return ""
		}
		return clean
	}

	// 解析全量 URL
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	// 提取 URL 路径并清洗
	cleanPath := strings.TrimPrefix(path.Clean(u.Path), "/")
	if r2Bucket != "" && strings.HasPrefix(cleanPath, r2Bucket+"/") {
		cleanPath = strings.TrimPrefix(cleanPath, r2Bucket+"/")
	}

	// 如果 URL 带有预签名参数 (X-Amz-*)，无论域名如何，其 Path 即为 ObjectKey
	if strings.Contains(raw, "X-Amz-") {
		return cleanPath
	}

	// 检查域名是否属于本桶
	isOurHost := false
	if r2PublicDomain != "" {
		if pubU, err := url.Parse(r2PublicDomain); err == nil && pubU.Host == u.Host {
			isOurHost = true
		}
	}
	if config.AppConfig.R2.Endpoint != "" {
		if epU, err := url.Parse(config.AppConfig.R2.Endpoint); err == nil && epU.Host == u.Host {
			isOurHost = true
		}
	}
	if strings.Contains(u.Host, "r2.cloudflarestorage.com") {
		isOurHost = true
	}

	if isOurHost {
		return cleanPath
	}

	// 属于外部图床或第三方 URL (如 unsplash.com)，保留原样
	return raw
}

// GeneratePresignedGetURL 为指定的相对路径 Key 生成带过期时间的临时预签名 GET 下载/访问 URL
func GeneratePresignedGetURL(objectKey string, customDuration ...time.Duration) (signedURL string, expiresIn int, err error) {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return "", 0, errors.New("object_key 不能为空")
	}

	duration := GetPresignExpireDuration()
	if len(customDuration) > 0 && customDuration[0] > 0 {
		duration = customDuration[0]
	}
	expiresIn = int(duration.Seconds())

	// 如果本身就是外部完整的 http(s) URL，则直接返回
	if (strings.HasPrefix(objectKey, "http://") || strings.HasPrefix(objectKey, "https://")) &&
		!strings.Contains(objectKey, "r2.cloudflarestorage.com") &&
		!strings.Contains(objectKey, "X-Amz-") {
		return objectKey, expiresIn, nil
	}

	// 未配置 R2 时回退本地或公共域名代理
	if !isConfigured || presignClient == nil {
		if r2PublicDomain != "" {
			return fmt.Sprintf("%s/%s", strings.TrimRight(r2PublicDomain, "/"), path.Clean(objectKey)), expiresIn, nil
		}
		if config.AppConfig.R2.Endpoint != "" && r2Bucket != "" {
			return fmt.Sprintf("%s/%s/%s", strings.TrimRight(config.AppConfig.R2.Endpoint, "/"), r2Bucket, path.Clean(objectKey)), expiresIn, nil
		}
		return "/assets/" + path.Clean(objectKey), expiresIn, nil
	}

	cleanKey := strings.TrimPrefix(path.Clean(objectKey), "/")
	if strings.HasPrefix(objectKey, "http://") || strings.HasPrefix(objectKey, "https://") {
		cleanKey = ExtractObjectKey(objectKey)
	}
	getInput := &s3.GetObjectInput{
		Bucket: aws.String(r2Bucket),
		Key:    aws.String(cleanKey),
	}

	presignedReq, err := presignClient.PresignGetObject(context.TODO(), getInput, s3.WithPresignExpires(duration))
	if err != nil {
		return "", 0, fmt.Errorf("failed to presign GET request: %w", err)
	}

	return presignedReq.URL, expiresIn, nil
}

// SignResourceURL 快捷函数：将相对路径或对象 Key 转换为有时效的临时签名可访问 URL
// 默认时效来自系统配置 (默认 30 分钟)。若生成失败或未配置 R2，智能回退为公开地址或原值
func SignResourceURL(resourcePathOrKey string, customDuration ...time.Duration) string {
	resourcePathOrKey = strings.TrimSpace(resourcePathOrKey)
	if resourcePathOrKey == "" {
		return ""
	}

	// 若已经是外部非 R2 的 HTTP/HTTPS 链接，且不带签名，保持原样
	if (strings.HasPrefix(resourcePathOrKey, "http://") || strings.HasPrefix(resourcePathOrKey, "https://")) &&
		!strings.Contains(resourcePathOrKey, "r2.cloudflarestorage.com") &&
		!strings.Contains(resourcePathOrKey, "X-Amz-") {
		if r2PublicDomain == "" || !strings.Contains(resourcePathOrKey, r2PublicDomain) {
			return resourcePathOrKey
		}
	}

	key := ExtractObjectKey(resourcePathOrKey)
	if key == "" {
		return resourcePathOrKey
	}

	url, _, err := GeneratePresignedGetURL(key, customDuration...)
	if err != nil {
		return GetPublicURL(key)
	}
	return url
}

// UploadObject 服务器端直接上传流到 R2，并在未配置 R2 时自动平滑回退写入本地存储
func UploadObject(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return errors.New("object_key 不能为空")
	}

	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if isConfigured && s3Client != nil {
		putInput := &s3.PutObjectInput{
			Bucket:        aws.String(r2Bucket),
			Key:           aws.String(objectKey),
			Body:          reader,
			ContentLength: aws.Int64(size),
			ContentType:   aws.String(contentType),
		}
		_, err := s3Client.PutObject(ctx, putInput)
		return err
	}

	// 离线/本地环境兜底写入 data/uploads/<objectKey>
	localBase := filepath.Join("data", "uploads")
	targetPath := filepath.Join(localBase, filepath.FromSlash(objectKey))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("failed to create upload dir: %w", err)
	}

	outFile, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer outFile.Close()

	if _, err := io.Copy(outFile, reader); err != nil {
		return fmt.Errorf("failed to write local file: %w", err)
	}

	return nil
}

// BuildSafeObjectKey 构建安全、合规的 R2 存储相对路径 Key (例如 avatar/user.png, entries/2026/10/09/uuid-file.png)
func BuildSafeObjectKey(folder, filename, customKey string) (string, error) {
	customKey = strings.TrimSpace(customKey)
	if customKey != "" {
		clean := path.Clean(strings.Trim(customKey, "/\\"))
		if strings.Contains(clean, "..") || strings.HasPrefix(clean, "/") {
			return "", errors.New("自定义 key 含有非法路径穿越字符")
		}
		return clean, nil
	}

	cleanName := sanitizeFilename(filename)
	uid := uuid.New().String()[:8]
	folder = strings.Trim(folder, "/\\")

	if folder == "avatar" {
		return fmt.Sprintf("avatar/%s-%s", uid, cleanName), nil
	}

	now := time.Now()
	if folder != "" && folder != "entries" {
		return fmt.Sprintf("%s/%04d/%02d/%s-%s", folder, now.Year(), now.Month(), uid, cleanName), nil
	}

	return fmt.Sprintf("entries/%04d/%02d/%02d/%s-%s",
		now.Year(), now.Month(), now.Day(), uid, cleanName), nil
}
