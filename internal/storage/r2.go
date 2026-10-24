package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3config "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

type presignedCacheItem struct {
	url       string
	expiresAt time.Time
}

var (
	// runtimeR2 是当前生效的 R2 配置快照。配置热更新与并发读（签名/状态）
	// 必须走这把锁，不能直接读写 config.AppConfig.R2（普通结构体赋值会撕裂）。
	runtimeR2Mu sync.RWMutex
	runtimeR2   config.R2Config
)

var (
	s3Mu           sync.RWMutex
	s3Client       *s3.Client
	presignClient  *s3.PresignClient
	r2Bucket       string
	r2PublicDomain string
	isConfigured   bool

	presignCacheMu sync.RWMutex
	presignCache   = make(map[string]presignedCacheItem)

	safeFilenameRegex = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
)

// ClearPresignCache 清理预签名 URL 缓存
func ClearPresignCache() {
	presignCacheMu.Lock()
	presignCache = make(map[string]presignedCacheItem)
	presignCacheMu.Unlock()
}

// r2Clients 当前 S3 客户端与桶信息的一致快照。
//
// 关键（发现 11）：isConfigured / s3Client / presignClient / r2Bucket / r2PublicDomain
// 是会被 InitR2 热更新（UpdateStorageConfig / ResetStorageConfig）并发改写的全局，
// 任何读用都必须先经 r2ClientsSnapshot() 在 s3Mu 读锁内取快照，再**只使用局部副本**。
// 直接裸读既是数据竞争（-race 必报），也可能拼出「新客户端 + 旧桶名」的撕裂组合。
type r2Clients struct {
	client       *s3.Client
	presign      *s3.PresignClient
	bucket       string
	publicDomain string
	configured   bool
}

func r2ClientsSnapshot() r2Clients {
	s3Mu.RLock()
	defer s3Mu.RUnlock()
	return r2Clients{
		client:       s3Client,
		presign:      presignClient,
		bucket:       r2Bucket,
		publicDomain: r2PublicDomain,
		configured:   isConfigured,
	}
}

// TestR2Connection 测试指定的 R2 / S3 配置是否能够正常通信，成功时返回握手往返延迟毫秒数
func TestR2Connection(ctx context.Context, cfg config.R2Config) (int64, error) {
	if !cfg.IsConfigured() {
		return 0, errors.New("配置信息不完整，必须填写 Endpoint (或 Account ID)、Access Key ID、Secret Access Key 与 Bucket Name")
	}

	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:               cfg.Endpoint,
			HostnameImmutable: true,
			SigningRegion:     "auto",
		}, nil
	})

	awsCfg, err := s3config.LoadDefaultConfig(ctx,
		s3config.WithEndpointResolverWithOptions(customResolver),
		s3config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
		s3config.WithRegion("auto"),
	)
	if err != nil {
		return 0, fmt.Errorf("加载 AWS S3 客户端配置失败: %w", err)
	}

	testClient := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
	testCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	start := time.Now()
	_, err = testClient.ListObjectsV2(testCtx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(cfg.BucketName),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "handshake failure") || strings.Contains(errMsg, "alert number 40") {
			return 0, fmt.Errorf("Cloudflare 拒绝访问：Account ID (%s) 无效", cfg.AccountID)
		}
		return 0, fmt.Errorf("对象存储连接校验失败: %w", err)
	}

	return time.Since(start).Milliseconds(), nil
}

// R2ConfigSnapshot 返回当前生效的 R2 配置（并发安全）。
func R2ConfigSnapshot() config.R2Config {
	runtimeR2Mu.RLock()
	defer runtimeR2Mu.RUnlock()
	return runtimeR2
}

// ReplaceR2Config 原子替换运行时配置并重建客户端。
// 必须先完成客户端重建再发布配置？不需要：读方拿到新配置时，
// InitR2 已持 s3Mu 完成切换；短暂窗口内签名使用旧客户端也仍可工作。
func ReplaceR2Config(cfg config.R2Config) error {
	if err := InitR2(cfg); err != nil {
		return err
	}
	runtimeR2Mu.Lock()
	config.AppConfig.R2 = cfg
	runtimeR2Mu.Unlock()
	return nil
}

// InitR2 初始化 Cloudflare R2 / S3 客户端
func InitR2(cfg config.R2Config) error {
	s3Mu.Lock()
	defer s3Mu.Unlock()

	if !cfg.IsConfigured() {
		isConfigured = false
		s3Client = nil
		presignClient = nil
		r2Bucket = ""
		r2PublicDomain = ""
		ClearPresignCache()
		runtimeR2Mu.Lock()
		runtimeR2 = cfg
		runtimeR2Mu.Unlock()
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

	s3Client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})
	presignClient = s3.NewPresignClient(s3Client)
	r2Bucket = cfg.BucketName
	r2PublicDomain = cfg.PublicDomain
	isConfigured = true
	ClearPresignCache()
	runtimeR2Mu.Lock()
	runtimeR2 = cfg
	runtimeR2Mu.Unlock()

	return nil
}

// IsConfigured 检查 R2 是否已配置就绪
func IsConfigured() bool {
	s3Mu.RLock()
	defer s3Mu.RUnlock()
	return isConfigured
}

// 存储状态中的本地文件统计缓存（BE-7）。
//
// 公开的 /storage/status 原先每个请求都在 s3Mu.RLock 内 filepath.Walk 整个上传目录，
// 文件多了以后既慢又会长时间占着存储锁。现在：
//   - 统计结果进程内缓存 60 秒；
//   - Walk 在 s3Mu 之外执行（只锁统计缓存，避免并发重复 Walk）；
//   - 上传/删除/迁移后主动失效。
const storageStatsTTL = 60 * time.Second

var (
	storageStatsMu         sync.Mutex
	storageStatsComputedAt time.Time
	storageStatsFileCount  int64
	storageStatsTotalBytes int64

	// storageWalk 可在测试中替换为计数器，验证缓存命中不重复遍历。
	storageWalk = filepath.Walk
)

// InvalidateStorageStatsCache 让本地文件统计缓存立即失效。
func InvalidateStorageStatsCache() {
	storageStatsMu.Lock()
	storageStatsComputedAt = time.Time{}
	storageStatsMu.Unlock()
}

// getLocalStorageStats 返回缓存中的文件数/字节数；缓存过期时才 Walk。
// 锁覆盖 Walk，保证并发请求只会触发一次遍历。
func getLocalStorageStats() (int64, int64) {
	storageStatsMu.Lock()
	defer storageStatsMu.Unlock()

	if !storageStatsComputedAt.IsZero() && time.Since(storageStatsComputedAt) < storageStatsTTL {
		return storageStatsFileCount, storageStatsTotalBytes
	}

	var fileCount, totalBytes int64
	localDir := GetLocalUploadDir()
	if fi, err := os.Stat(localDir); err == nil && fi.IsDir() {
		_ = storageWalk(localDir, func(p string, info os.FileInfo, err error) error {
			if err == nil && info != nil && !info.IsDir() {
				fileCount++
				totalBytes += info.Size()
			}
			return nil
		})
	}

	storageStatsFileCount = fileCount
	storageStatsTotalBytes = totalBytes
	storageStatsComputedAt = time.Now()
	return fileCount, totalBytes
}

// GetStorageStatusBasic 非管理员公开状态：不触发任何磁盘遍历。
func GetStorageStatusBasic() map[string]interface{} {
	s3Mu.RLock()
	configured := isConfigured
	s3Mu.RUnlock()
	return map[string]interface{}{
		"provider":   "Cloudflare R2",
		"configured": configured,
	}
}

// GetStorageStatus 获取存储服务当前公开状态 (不暴露密钥)
func GetStorageStatus() map[string]interface{} {
	// 只把 isConfigured 的读取放进 s3Mu；磁盘统计在锁外进行（BE-7）。
	s3Mu.RLock()
	configured := isConfigured
	s3Mu.RUnlock()

	cfg := R2ConfigSnapshot()
	expireSec := cfg.PresignExpireSeconds
	if expireSec <= 0 {
		expireSec = 1800
	}

	localFileCount, localTotalBytes := getLocalStorageStats()

	return map[string]interface{}{
		"provider":               "Cloudflare R2",
		"configured":             configured,
		"account_id":             cfg.AccountID,
		"bucket":                 cfg.BucketName,
		"public_domain":          cfg.PublicDomain,
		"endpoint":               cfg.Endpoint,
		"access_key_id":          cfg.AccessKeyID,
		"has_secret_key":         cfg.SecretAccessKey != "",
		"presign_expire_seconds": expireSec,
		"presign_expire_minutes": expireSec / 60,
		"local_file_count":       localFileCount,
		"local_total_bytes":      localTotalBytes,
	}
}

// GetPresignExpireDuration 获取预签名临时下载 URL 的配置有效时长 (默认 30 分钟 / 1800 秒)
func GetPresignExpireDuration() time.Duration {
	sec := R2ConfigSnapshot().PresignExpireSeconds
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
	snap := r2ClientsSnapshot()
	if !snap.configured || snap.presign == nil {
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
		Bucket:        aws.String(snap.bucket),
		Key:           aws.String(objectKey),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	}

	presignedReq, err := snap.presign.PresignPutObject(context.TODO(), putInput, s3.WithPresignExpires(expiresDuration))
	if err != nil {
		return "", "", "", 0, fmt.Errorf("failed to presign PUT request: %w", err)
	}

	return uid, objectKey, presignedReq.URL, expiresIn, nil
}

// VerifyObjectExists 确认对象已实际上传到 R2，并获取真实大小与类型
func VerifyObjectExists(objectKey string) (size int64, contentType string, err error) {
	snap := r2ClientsSnapshot()
	if !snap.configured || snap.client == nil {
		return 0, "", errors.New("R2 存储未配置")
	}

	headInput := &s3.HeadObjectInput{
		Bucket: aws.String(snap.bucket),
		Key:    aws.String(objectKey),
	}

	res, err := snap.client.HeadObject(context.TODO(), headInput)
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

// DeleteObject 删除指定对象：本地主存储与 R2 备份都要清掉。
//
// 关键：不能因为 R2 未配置就直接 return —— 本地磁盘才是主存储，
// 否则「物理删除附件」之后原图仍留在 VPS 上，旧签名 URL 继续可读。
func DeleteObject(objectKey string) error {
	if objectKey == "" {
		return nil
	}
	localErr := DeleteLocalObject(objectKey)
	snap := r2ClientsSnapshot()
	if !snap.configured || snap.client == nil {
		return localErr
	}

	delInput := &s3.DeleteObjectInput{
		Bucket: aws.String(snap.bucket),
		Key:    aws.String(objectKey),
	}

	_, err := snap.client.DeleteObject(context.TODO(), delInput)
	// 本地统计缓存一并失效：删除动作即使只影响 R2，也避免状态页继续展示旧口径。
	InvalidateStorageStatsCache()
	return errors.Join(localErr, err)
}

// DeleteObjectWithContext 带 context 的删除（长任务/purge 提交后清理用）。
func DeleteObjectWithContext(ctx context.Context, objectKey string) error {
	if objectKey == "" {
		return nil
	}
	localErr := DeleteLocalObject(objectKey)
	snap := r2ClientsSnapshot()
	if !snap.configured || snap.client == nil {
		return localErr
	}
	delInput := &s3.DeleteObjectInput{
		Bucket: aws.String(snap.bucket),
		Key:    aws.String(objectKey),
	}
	_, err := snap.client.DeleteObject(ctx, delInput)
	InvalidateStorageStatsCache()
	return errors.Join(localErr, err)
}

// S3ObjectItem 表示对象存储中的对象元数据
type S3ObjectItem struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ListObjects 列出指定前缀的对象
func ListObjects(ctx context.Context, prefix string) ([]S3ObjectItem, error) {
	snap := r2ClientsSnapshot()
	if !snap.configured || snap.client == nil {
		return nil, nil
	}
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(snap.bucket),
		Prefix: aws.String(prefix),
	}
	output, err := snap.client.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, err
	}
	var items []S3ObjectItem
	for _, obj := range output.Contents {
		key := aws.ToString(obj.Key)
		lastMod := time.Time{}
		if obj.LastModified != nil {
			lastMod = *obj.LastModified
		}
		items = append(items, S3ObjectItem{
			Key:          key,
			Size:         aws.ToInt64(obj.Size),
			LastModified: lastMod,
		})
	}
	return items, nil
}

// GetObject 获取 R2 存储对象的读取流
func GetObject(ctx context.Context, objectKey string) (io.ReadCloser, int64, error) {
	snap := r2ClientsSnapshot()
	if !snap.configured || snap.client == nil {
		return nil, 0, errors.New("对象存储未配置")
	}
	input := &s3.GetObjectInput{
		Bucket: aws.String(snap.bucket),
		Key:    aws.String(objectKey),
	}
	output, err := snap.client.GetObject(ctx, input)
	if err != nil {
		return nil, 0, err
	}
	return output.Body, aws.ToInt64(output.ContentLength), nil
}

// GetPublicURL 计算公开访问 URL (用于静态无鉴权降级场景)
func GetPublicURL(objectKey string) string {
	if objectKey == "" {
		return ""
	}
	if strings.HasPrefix(objectKey, "http://") || strings.HasPrefix(objectKey, "https://") {
		return objectKey
	}
	snap := r2ClientsSnapshot()
	if snap.publicDomain != "" {
		return fmt.Sprintf("%s/%s", strings.TrimRight(snap.publicDomain, "/"), escapeObjectKeyPath(path.Clean(objectKey)))
	}
	cfg := R2ConfigSnapshot()
	if cfg.Endpoint != "" && snap.bucket != "" {
		return fmt.Sprintf("%s/%s/%s", strings.TrimRight(cfg.Endpoint, "/"), snap.bucket, escapeObjectKeyPath(path.Clean(objectKey)))
	}
	return "/media/" + escapeObjectKeyPath(strings.TrimPrefix(path.Clean(objectKey), "/"))
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
		// 只有中枢媒体路由或带查询串的 URL 路径需要还原转义；
		// 原始 object_key 必须保持字面值 —— 否则键里的字面 %20 会被改成空格，
		// 指向一个完全不同的对象，且写入/读取不再互逆。
		if strings.HasPrefix(raw, "/media/") || strings.Contains(raw, "?") {
			if decoded, err := url.PathUnescape(clean); err == nil {
				clean = decoded
			}
		}
		return clean
	}

	snap := r2ClientsSnapshot()

	// 解析全量 URL
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	// 提取 URL 路径并清洗
	cleanPath := strings.TrimPrefix(path.Clean(u.Path), "/")
	if snap.bucket != "" && strings.HasPrefix(cleanPath, snap.bucket+"/") {
		cleanPath = strings.TrimPrefix(cleanPath, snap.bucket+"/")
	}

	// 如果 URL 带有预签名参数 (X-Amz-*)，无论域名如何，其 Path 即为 ObjectKey
	if strings.Contains(raw, "X-Amz-") {
		return cleanPath
	}

	// 检查域名是否属于本桶
	isOurHost := false
	if snap.publicDomain != "" {
		if pubU, err := url.Parse(snap.publicDomain); err == nil && pubU.Host == u.Host {
			isOurHost = true
		}
	}
	cfg := R2ConfigSnapshot()
	if cfg.Endpoint != "" {
		if epU, err := url.Parse(cfg.Endpoint); err == nil && epU.Host == u.Host {
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

	// 未配置 R2 时回退本地或公共域名代理 (RC-44: 本地媒体统一走 /media 前缀)
	snap := r2ClientsSnapshot()
	if !snap.configured || snap.presign == nil {
		if snap.publicDomain != "" {
			return fmt.Sprintf("%s/%s", strings.TrimRight(snap.publicDomain, "/"), escapeObjectKeyPath(path.Clean(objectKey))), expiresIn, nil
		}
		cfg := R2ConfigSnapshot()
		if cfg.Endpoint != "" && snap.bucket != "" {
			return fmt.Sprintf("%s/%s/%s", strings.TrimRight(cfg.Endpoint, "/"), snap.bucket, escapeObjectKeyPath(path.Clean(objectKey))), expiresIn, nil
		}
		return "/media/" + escapeObjectKeyPath(strings.TrimPrefix(path.Clean(objectKey), "/")), expiresIn, nil
	}

	cleanKey := strings.TrimPrefix(path.Clean(objectKey), "/")
	if strings.HasPrefix(objectKey, "http://") || strings.HasPrefix(objectKey, "https://") {
		cleanKey = ExtractObjectKey(objectKey)
	}

	// 内存复用近期签发有效凭证，消除响应体动态扰动，确保 ETag / 304 缓存生效 (BE-04)
	cacheKey := fmt.Sprintf("%s|%d", cleanKey, expiresIn)
	// 年鉴/列表的 ETag 缓存窗口为 25 分钟：复用旧签名时必须保证交给客户端的
	// 剩余有效期覆盖该窗口，否则 304 缓存里的图片会在下一次刷新前先失效。
	minRemaining := 25 * time.Minute
	if duration <= minRemaining {
		minRemaining = duration / 2
	}
	presignCacheMu.RLock()
	if item, ok := presignCache[cacheKey]; ok {
		if remaining := time.Until(item.expiresAt); remaining >= minRemaining {
			presignCacheMu.RUnlock()
			return item.url, int(remaining.Seconds()), nil
		}
	}
	presignCacheMu.RUnlock()

	getInput := &s3.GetObjectInput{
		Bucket: aws.String(snap.bucket),
		Key:    aws.String(cleanKey),
	}

	presignedReq, err := snap.presign.PresignGetObject(context.TODO(), getInput, s3.WithPresignExpires(duration))
	if err != nil {
		return "", 0, fmt.Errorf("failed to presign GET request: %w", err)
	}

	presignCacheMu.Lock()
	now := time.Now()
	// 定期淘汰过期项并硬限制容量上限，避免无界累积 (BE-04)
	if len(presignCache) > 1000 {
		for k, v := range presignCache {
			if now.After(v.expiresAt) {
				delete(presignCache, k)
			}
		}
		if len(presignCache) > 2000 {
			for k := range presignCache {
				delete(presignCache, k)
				if len(presignCache) <= 1000 {
					break
				}
			}
		}
	}
	presignCache[cacheKey] = presignedCacheItem{
		url:       presignedReq.URL,
		expiresAt: now.Add(duration),
	}
	presignCacheMu.Unlock()

	return presignedReq.URL, expiresIn, nil
}

// SignResourceURL 快捷函数：将相对路径或对象 Key 转换为有时效的临时签名可访问 URL
// 默认时效来自系统配置 (默认 30 分钟)。若生成失败或未配置 R2，智能回退为公开地址或原值
func SignResourceURL(resourcePathOrKey string, customDuration ...time.Duration) string {
	resourcePathOrKey = strings.TrimSpace(resourcePathOrKey)
	if resourcePathOrKey == "" {
		return ""
	}

	// 本地为主：文件就在同一台机器的磁盘上 → 给中枢自己的 /media 路由。
	// 关键收益：客户端不必绕道对象存储。实测同一张 1.5MB 的相纸，
	// 走对象存储要 4.4 秒，走本机路由是毫秒级。
	//
	// 关键：给的是**带签名的** URL（与对象存储的预签名同构），
	// 而不是一个裸路径 —— 否则 /media 就是无鉴权公开目录，
	// 任何知道中枢地址的人都能翻阅全部照片。
	if localPrimaryEnabled {
		key := ExtractObjectKey(resourcePathOrKey)
		if key != "" && ObjectExistsLocally(key) {
			return SignLocalMediaURL(key, customDuration...)
		}
	}

	// 若已经是外部非 R2 的 HTTP/HTTPS 链接，且不带签名，保持原样
	if (strings.HasPrefix(resourcePathOrKey, "http://") || strings.HasPrefix(resourcePathOrKey, "https://")) &&
		!strings.Contains(resourcePathOrKey, "r2.cloudflarestorage.com") &&
		!strings.Contains(resourcePathOrKey, "X-Amz-") {
		pubDomain := r2ClientsSnapshot().publicDomain
		if pubDomain == "" || !strings.Contains(resourcePathOrKey, pubDomain) {
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

var (
	localUploadDirMu sync.RWMutex
	localUploadDir   = filepath.Join("data", "uploads")
)

// SetLocalUploadDir 动态设置本地媒体上传根目录 (支持单元测试隔离到临时目录 RC-68)
func SetLocalUploadDir(dir string) {
	localUploadDirMu.Lock()
	defer localUploadDirMu.Unlock()
	localUploadDir = dir
}

// GetLocalUploadDir 获取当前本地媒体上传根目录
func GetLocalUploadDir() string {
	localUploadDirMu.RLock()
	defer localUploadDirMu.RUnlock()
	return localUploadDir
}

// UploadObject 写入对象：**本地磁盘为主，R2 为备份**
//
// 关键（部署形态：中枢在 VPS 上，真正存储是 VPS 本地磁盘，R2 只做备份）：
//   - 本地写入是**必须成功**的主路径
//   - R2 写入是**尽力而为**的备份：失败只记日志，不影响上传结果
//     （备份失败不该让用户「发不出照片」）
//   - 读取与 URL 生成另有 `ReadObject` / `SignResourceURL` 统一按「本地优先」处理
//
// 旧行为（R2 为主）仍可通过环境变量 `LOCAL_MEDIA_PRIMARY=0` 保留。
func UploadObject(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return errors.New("object_key 不能为空")
	}

	if contentType == "" {
		contentType = "application/octet-stream"
	}

	snap := r2ClientsSnapshot()
	r2Ready := snap.configured && snap.client != nil

	// 旧行为：R2 为主（且不写本地）
	if !localPrimaryEnabled {
		if r2Ready {
			putInput := &s3.PutObjectInput{
				Bucket:        aws.String(snap.bucket),
				Key:           aws.String(objectKey),
				Body:          reader,
				ContentLength: aws.Int64(size),
				ContentType:   aws.String(contentType),
			}
			_, err := snap.client.PutObject(ctx, putInput)
			return err
		}
		return writeLocalObject(objectKey, reader, size)
	}

	// 新行为：本地为主
	// 读入内存一份，好让 R2 备份能复用同一份字节（流只能读一次）
	var payload []byte
	if r2Ready {
		limited := io.LimitReader(reader, 512<<20)
		var readErr error
		payload, readErr = io.ReadAll(limited)
		if readErr != nil {
			return fmt.Errorf("failed to read upload payload: %w", readErr)
		}
		if size > 0 && int64(len(payload)) != size {
			return fmt.Errorf("上传字节数不符：期望 %d，实际 %d", size, len(payload))
		}
		if err := writeLocalObject(objectKey, bytes.NewReader(payload), int64(len(payload))); err != nil {
			return err
		}
		// 备份：尽力而为
		putInput := &s3.PutObjectInput{
			Bucket:        aws.String(snap.bucket),
			Key:           aws.String(objectKey),
			Body:          bytes.NewReader(payload),
			ContentLength: aws.Int64(int64(len(payload))),
			ContentType:   aws.String(contentType),
		}
		if _, err := snap.client.PutObject(ctx, putInput); err != nil {
			// 备份失败不影响主流程，但必须留下痕迹
			log.Printf("[storage] R2 备份失败（本地已写入成功）：key=%s err=%v", objectKey, err)
		}
		return nil
	}

	// 未配置 R2：只写本地
	return writeLocalObject(objectKey, reader, size)
}

// escapeObjectKeyPath 对对象键逐段做 URL 转义（保留 `/` 分隔符）。
// 对象键可能包含空格 / `#` / `%` 等字符，直接拼进 URL 会被浏览器或反代截断；
// 逐段 PathEscape 后既能正确访问，也不会把目录分隔符转义掉。
func escapeObjectKeyPath(objectKey string) string {
	segments := strings.Split(strings.TrimPrefix(objectKey, "/"), "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return strings.Join(segments, "/")
}

// BuildSafeObjectKey 构建安全、合规的 R2 存储相对路径 Key (例如 avatar/user.png, entries/2026/10/09/uuid-file.png)
// folder 与 customKey 一律做 Clean + 拒绝路径穿越字符，杜绝写出 data/uploads 之外 (BE-22)
func BuildSafeObjectKey(folder, filename, customKey string) (string, error) {
	customKey = strings.TrimSpace(customKey)
	if customKey != "" {
		clean := path.Clean(strings.Trim(customKey, "/\\"))
		if strings.Contains(clean, "..") || strings.HasPrefix(clean, "/") ||
			strings.ContainsAny(clean, "?#%\\\x00\r\n") {
			return "", errors.New("自定义 key 含有非法字符")
		}
		folder = strings.TrimSpace(folder)
		if folder != "" && clean != folder && !strings.HasPrefix(clean, folder+"/") {
			return "", errors.New("自定义 key 必须位于 " + folder + "/ 目录内")
		}
		segs := strings.Split(clean, "/")
		for i := range segs {
			if segs[i] == "" || segs[i] == "." || segs[i] == ".." {
				return "", errors.New("自定义 key 含有非法路径段")
			}
		}
		// 只净化最后一段文件名，目录结构保持原样（白名单与穿越检查已通过）。
		segs[len(segs)-1] = sanitizeFilename(segs[len(segs)-1])
		return strings.Join(segs, "/"), nil
	}

	cleanName := sanitizeFilename(filename)
	uid := uuid.New().String()[:8]
	folder = strings.TrimSpace(folder)
	if folder != "" {
		cleanFolder := path.Clean(strings.Trim(folder, "/\\"))
		if cleanFolder == "." || cleanFolder == ".." ||
			strings.Contains(cleanFolder, "..") || strings.HasPrefix(cleanFolder, "/") {
			return "", errors.New("存储目录含有非法路径穿越字符")
		}
		folder = cleanFolder
	}

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
