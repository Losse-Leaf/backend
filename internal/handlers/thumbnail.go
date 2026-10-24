// thumbnail.go
// 附件缩略图服务
//
// 背景：时间流与详情页里的一张「相纸」，原始文件动辄 1.5MB（2412×1584 JPEG），
// 从对象存储拉一次要 4 秒以上。客户端为了显示 220pt 高的缩略图，
// 却必须先把**整张原图**下载下来再本地降采样 —— 于是：
//   - 打开一条带图的散页 → 转圈好几秒
//   - 滚动时间流 → 每张新卡片都在拉 1.5MB，网络与解码把主线程打满
//
// 解决：后端出缩略图路由，列表/详情用缩略图，只有灯箱（点开大图）才用原图。
//
// 关键设计：
//   - 缩略图走**中枢自己的路由**（而不是对象存储的预签名 URL），
//     因此 URL 稳定 → 客户端缓存键稳定 → 缓存能命中
//   - 服务端内存 + 磁盘双层缓存，缩略图算一次就复用
//   - R2 与本地磁盘两条通道都支持

package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/image/draw"

	"loose-leaf/internal/middleware"
	"loose-leaf/internal/storage"
)

// 缩略图参数约束
const (
	// 默认缩略图宽度（px）：足以覆盖 220pt 高的相纸框（Retina 下约 440px）
	thumbnailDefaultWidth = 480
	// 最大宽度：防止有人请求 8000px 把内存打爆
	thumbnailMaxWidth = 1600
	// JPEG 质量：肉眼无差别、体积大幅下降
	thumbnailQuality = 72
	// 内存缓存条目上限
	thumbnailMemoryEntries = 256
	// 源图安全上限：单边 12000px、总像素 50MP（BE-6）
	thumbnailMaxSourceSide   = 12000
	thumbnailMaxSourcePixels = 50_000_000
	// Cache-Control 不得超过缩略图签名 TTL（30 分钟）
	thumbnailCacheMaxAge = 1800
)

// thumbnailDecodeHook 仅供测试统计实际解码次数（并发去重验收）。
var thumbnailDecodeHook func()

// thumbnailInflightCall 合并同一 cacheKey 的并发生成：
// 第一个请求计算，其余请求等待同一结果；失败同样广播。
type thumbnailInflightCall struct {
	done   chan struct{}
	data   []byte
	status int
	err    error
}

var (
	thumbnailInflightMu sync.Mutex
	thumbnailInflight   = map[string]*thumbnailInflightCall{}
)

// 缩略图内存缓存
type thumbnailEntry struct {
	data      []byte
	expiresAt time.Time
}

var (
	thumbnailMemoryMu sync.RWMutex
	thumbnailMemory   = map[string]thumbnailEntry{}
	// 缩略图磁盘缓存目录
	thumbnailDiskOnce sync.Once
	thumbnailDiskDir  string
)

// thumbnailCacheKey 缓存键：对象键 + 目标宽度
func thumbnailCacheKey(objectKey string, width int) string {
	// 本地主存储的同一 objectKey 可能被自定义 key 覆盖写（如 avatar/user.png）。
	// 把源文件的 size + mtime 混入缓存键与 ETag，覆盖后自动换新；
	// R2 模式取不到本地文件时为空版本，行为与之前一致。
	version := ""
	if p := storage.LocalObjectPath(objectKey); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			version = fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
		}
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s@%d@%s", objectKey, width, version)))
	return hex.EncodeToString(sum[:])
}

// thumbnailDiskPath 磁盘缓存路径（两级散列目录，避免单目录文件过多）
//
// 关键：必须先调 `ensureThumbnailDiskDir()` —— 早先直接用还没初始化的
// `thumbnailDiskDir`（空串），于是 `filepath.Join("", "9f")` 拼出**相对路径**，
// 缓存文件就落在进程工作目录里（源码树中会冒出 `9f/`、`57/` 之类的杂物目录）。
func thumbnailDiskPath(objectKey string, width int) string {
	key := thumbnailCacheKey(objectKey, width)
	dir := filepath.Join(ensureThumbnailDiskDir(), key[:2])
	return filepath.Join(dir, key+".jpg")
}

// ensureThumbnailDiskDir 惰性初始化磁盘缓存目录
//
// 关键：目录必须是**绝对路径**。
// 早先用相对路径（`data/uploads/.thumbnails`），于是缓存目录会随进程工作目录
// 漂移 —— `go test` 在包目录下跑就会在源码树里留下 `9f/`、`57/` 之类的杂物目录。
func ensureThumbnailDiskDir() string {
	thumbnailDiskOnce.Do(func() {
		base := storage.GetLocalUploadDir()
		if base == "" {
			base = filepath.Join(os.TempDir(), "looseleaf")
		}
		if abs, err := filepath.Abs(base); err == nil {
			base = abs
		}
		thumbnailDiskDir = filepath.Join(base, ".thumbnails")
		_ = os.MkdirAll(thumbnailDiskDir, 0o700)
	})
	return thumbnailDiskDir
}

// readObjectBytes 读取对象原始字节：**本地优先**，本地没有才回落对象存储
func readObjectBytes(ctx context.Context, objectKey string) ([]byte, error) {
	return storage.ReadObject(ctx, objectKey)
}

// makeThumbnail 生成缩略图字节
//
// BE-6：先 DecodeConfig 做尺寸/像素上限校验，再完整 Decode。
// 否则一张 30000×30000 的源图会先被解码成数 GB 的 RGBA，进程直接 OOM。
func makeThumbnail(raw []byte, targetWidth int) ([]byte, error) {
	if thumbnailDecodeHook != nil {
		thumbnailDecodeHook()
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("图片解码失败: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("图片尺寸非法")
	}
	if cfg.Width > thumbnailMaxSourceSide || cfg.Height > thumbnailMaxSourceSide {
		return nil, fmt.Errorf("源图尺寸过大: %dx%d（单边上限 %dpx）", cfg.Width, cfg.Height, thumbnailMaxSourceSide)
	}
	if int64(cfg.Width)*int64(cfg.Height) > thumbnailMaxSourcePixels {
		return nil, fmt.Errorf("源图像素过多: %dx%d = %.1fMP（上限 %dMP）",
			cfg.Width, cfg.Height,
			float64(cfg.Width)*float64(cfg.Height)/1_000_000,
			thumbnailMaxSourcePixels/1_000_000)
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("图片解码失败: %w", err)
	}
	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, fmt.Errorf("图片尺寸非法")
	}

	// 只在需要缩小时才缩放；已经很小就直接重编码
	dstW, dstH := srcW, srcH
	if srcW > targetWidth {
		dstW = targetWidth
		dstH = int(float64(srcH) * float64(targetWidth) / float64(srcW))
		if dstH < 1 {
			dstH = 1
		}
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: thumbnailQuality}); err != nil {
		return nil, fmt.Errorf("缩略图编码失败: %w", err)
	}
	return buf.Bytes(), nil
}

// ThumbnailFile 提供附件缩略图
//
// GET /api/v1/files/thumbnail?key=<object_key>&w=480
//
// 鉴权：与其它文件接口一致（已配对设备令牌）。
func ThumbnailFile(c *gin.Context) {
	objectKey := strings.TrimSpace(c.Query("key"))
	objectKey = storage.ExtractObjectKey(objectKey)
	if objectKey == "" {
		Error(c, http.StatusBadRequest, "缺少对象键 (key)")
		return
	}

	// 关键：校验签名（或有效的设备令牌 / 会话 Cookie）。
	// 缩略图与本机媒体是同一套访问控制 ——
	// 否则它就是一个公开的「按 key 取图」接口。
	width := thumbnailDefaultWidth
	if raw := strings.TrimSpace(c.Query("w")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			width = parsed
		}
	}
	if width > thumbnailMaxWidth {
		width = thumbnailMaxWidth
	}
	authorized := middleware.IsAdmin(c)
	if !authorized {
		authorized = storage.VerifyLocalThumbnailSignature(
			objectKey, width,
			c.Query("exp"),
			c.Query("sig"),
		)
	}
	if !authorized {
		Error(c, http.StatusUnauthorized, "需要有效签名或授权才能访问缩略图")
		return
	}

	cacheKey := thumbnailCacheKey(objectKey, width)

	// 1) 内存缓存
	if data, ok := thumbnailFromMemory(cacheKey); ok {
		serveThumbnail(c, data, cacheKey)
		return
	}

	// 2) 磁盘缓存
	diskPath := thumbnailDiskPath(objectKey, width)
	if data, err := os.ReadFile(diskPath); err == nil && len(data) > 0 {
		storeThumbnail(cacheKey, diskPath, data)
		serveThumbnail(c, data, cacheKey)
		return
	}

	// 3) 现算（同 key 并发合并，只解码一次）
	data, status, err := buildThumbnailOnce(c.Request.Context(), objectKey, width, cacheKey, diskPath)
	if err != nil {
		Error(c, status, err.Error())
		return
	}
	serveThumbnail(c, data, cacheKey)
}

func thumbnailFromMemory(cacheKey string) ([]byte, bool) {
	thumbnailMemoryMu.RLock()
	entry, ok := thumbnailMemory[cacheKey]
	thumbnailMemoryMu.RUnlock()
	if ok && entry.expiresAt.After(time.Now()) {
		return entry.data, true
	}
	return nil, false
}

func storeThumbnail(cacheKey, diskPath string, data []byte) {
	thumbnailMemoryMu.Lock()
	if len(thumbnailMemory) >= thumbnailMemoryEntries {
		// 简单淘汰：清掉最早到期的若干条
		now := time.Now()
		for k, v := range thumbnailMemory {
			if v.expiresAt.Before(now) {
				delete(thumbnailMemory, k)
			}
		}
		if len(thumbnailMemory) >= thumbnailMemoryEntries {
			thumbnailMemory = map[string]thumbnailEntry{}
		}
	}
	thumbnailMemory[cacheKey] = thumbnailEntry{data: data, expiresAt: time.Now().Add(30 * time.Minute)}
	thumbnailMemoryMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(diskPath), 0o700); err == nil {
		_ = os.WriteFile(diskPath, data, 0o600)
	}
}

// buildThumbnailOnce 合并同 cacheKey 的并发生成。
// leader 负责读源图/解码/落缓存；follower 等待同一个结果，失败也一并广播。
func buildThumbnailOnce(ctx context.Context, objectKey string, width int, cacheKey, diskPath string) ([]byte, int, error) {
	thumbnailInflightMu.Lock()
	if call, ok := thumbnailInflight[cacheKey]; ok {
		thumbnailInflightMu.Unlock()
		select {
		case <-call.done:
			return call.data, call.status, call.err
		case <-ctx.Done():
			return nil, http.StatusRequestTimeout, ctx.Err()
		}
	}
	call := &thumbnailInflightCall{done: make(chan struct{})}
	thumbnailInflight[cacheKey] = call
	thumbnailInflightMu.Unlock()

	defer func() {
		thumbnailInflightMu.Lock()
		delete(thumbnailInflight, cacheKey)
		thumbnailInflightMu.Unlock()
		close(call.done)
	}()

	// 抢到 leader 后重新看一眼缓存：上一个 leader 可能刚写完并退出 map。
	if data, ok := thumbnailFromMemory(cacheKey); ok {
		call.data, call.status = data, http.StatusOK
		return data, http.StatusOK, nil
	}
	if data, err := os.ReadFile(diskPath); err == nil && len(data) > 0 {
		storeThumbnail(cacheKey, diskPath, data)
		call.data, call.status = data, http.StatusOK
		return data, http.StatusOK, nil
	}

	// 共享计算不能绑定 leader 的请求生命周期：第一个请求方断开时，
	// 其他仍在等待的 follower 不应一起收到 404。给共享任务独立的超时。
	workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	raw, err := readObjectBytes(workCtx, objectKey)
	if err != nil {
		call.status, call.err = http.StatusNotFound, fmt.Errorf("缩略图源不可用: %w", err)
		return nil, call.status, call.err
	}
	data, err := makeThumbnail(raw, width)
	if err != nil {
		call.status, call.err = http.StatusUnprocessableEntity, err
		return nil, call.status, call.err
	}
	storeThumbnail(cacheKey, diskPath, data)
	call.data, call.status = data, http.StatusOK
	return data, http.StatusOK, nil
}

// serveThumbnail 输出缩略图；命中 If-None-Match 时返回 304 且不写 body。
func serveThumbnail(c *gin.Context, data []byte, cacheKey string) {
	etag := `"` + cacheKey[:32] + `"`
	c.Header("ETag", etag)
	// 关键：max-age 不得超过缩略图签名 TTL（30 分钟），
	// 否则签名过期后客户端仍拿着旧 URL 反复命中本地缓存，行为难以解释。
	c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d, immutable", thumbnailCacheMaxAge))

	if match := c.GetHeader("If-None-Match"); match != "" && match == etag {
		c.Status(http.StatusNotModified)
		return
	}

	c.Header("Content-Type", "image/jpeg")
	c.Header("Content-Length", strconv.Itoa(len(data)))
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write(data)
}

// ThumbnailURLFor 生成某个对象键的缩略图 URL（供附件列表填充给客户端）
func ThumbnailURLFor(objectKey string, width int) string {
	objectKey = storage.ExtractObjectKey(objectKey)
	if objectKey == "" {
		return ""
	}
	if width <= 0 {
		width = thumbnailDefaultWidth
	}
	// 关键：缩略图地址也**必须签名** ——
	// 否则它就是一个公开的「按 key 取图」接口，
	// 任何知道 object_key 的人都能拿到你的相纸。
	return storage.SignLocalThumbnailURL(objectKey, width)
}
