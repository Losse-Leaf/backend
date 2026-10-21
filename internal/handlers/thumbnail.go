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
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/image/draw"

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
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s@%d", objectKey, width)))
	return hex.EncodeToString(sum[:])
}

// thumbnailDiskPath 磁盘缓存路径（两级散列目录，避免单目录文件过多）
func thumbnailDiskPath(objectKey string, width int) string {
	key := thumbnailCacheKey(objectKey, width)
	dir := filepath.Join(thumbnailDiskDir, key[:2])
	return filepath.Join(dir, key+".jpg")
}

// ensureThumbnailDiskDir 惰性初始化磁盘缓存目录
func ensureThumbnailDiskDir() string {
	thumbnailDiskOnce.Do(func() {
		base := storage.GetLocalUploadDir()
		if base == "" {
			base = os.TempDir()
		}
		thumbnailDiskDir = filepath.Join(base, ".thumbnails")
		_ = os.MkdirAll(thumbnailDiskDir, 0o755)
	})
	return thumbnailDiskDir
}

// readObjectBytes 读取对象原始字节（R2 优先，未配置时走本地磁盘）
func readObjectBytes(ctx context.Context, objectKey string) ([]byte, error) {
	if body, _, err := storage.GetObject(ctx, objectKey); err == nil && body != nil {
		defer body.Close()
		return io.ReadAll(io.LimitReader(body, 64<<20))
	}
	// 本地磁盘兜底
	base := storage.GetLocalUploadDir()
	if base == "" {
		return nil, fmt.Errorf("对象存储与本地磁盘均不可用")
	}
	target := filepath.Join(base, filepath.FromSlash(objectKey))
	if absBase, err := filepath.Abs(base); err == nil {
		if absTarget, err := filepath.Abs(target); err == nil {
			if absTarget != absBase && !strings.HasPrefix(absTarget, absBase+string(filepath.Separator)) {
				return nil, fmt.Errorf("对象路径越界")
			}
		}
	}
	return os.ReadFile(target)
}

// makeThumbnail 生成缩略图字节
func makeThumbnail(raw []byte, targetWidth int) ([]byte, error) {
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

	width := thumbnailDefaultWidth
	if raw := strings.TrimSpace(c.Query("w")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			width = parsed
		}
	}
	if width > thumbnailMaxWidth {
		width = thumbnailMaxWidth
	}

	cacheKey := thumbnailCacheKey(objectKey, width)

	// 1) 内存缓存
	thumbnailMemoryMu.RLock()
	entry, ok := thumbnailMemory[cacheKey]
	thumbnailMemoryMu.RUnlock()
	if ok && entry.expiresAt.After(time.Now()) {
	 serveThumbnail(c, entry.data, cacheKey)
		return
	}

	// 2) 磁盘缓存
	diskPath := thumbnailDiskPath(objectKey, width)
	if data, err := os.ReadFile(diskPath); err == nil && len(data) > 0 {
		thumbnailMemoryMu.Lock()
		thumbnailMemory[cacheKey] = thumbnailEntry{data: data, expiresAt: time.Now().Add(30 * time.Minute)}
		thumbnailMemoryMu.Unlock()
		serveThumbnail(c, data, cacheKey)
		return
	}

	// 3) 现算
	raw, err := readObjectBytes(c.Request.Context(), objectKey)
	if err != nil {
		Error(c, http.StatusNotFound, "缩略图源不可用: "+err.Error())
		return
	}
	data, err := makeThumbnail(raw, width)
	if err != nil {
		Error(c, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// 4) 落缓存
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

	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err == nil {
		_ = os.WriteFile(diskPath, data, 0o644)
	}

	serveThumbnail(c, data, cacheKey)
}

// serveThumbnail 输出缩略图
func serveThumbnail(c *gin.Context, data []byte, cacheKey string) {
	c.Header("Content-Type", "image/jpeg")
	c.Header("Content-Length", strconv.Itoa(len(data)))
	// 缩略图内容稳定（键 = 对象键 + 宽度），可以长期缓存
	c.Header("Cache-Control", "public, max-age=86400, immutable")
	c.Header("ETag", `"`+cacheKey[:32]+`"`)
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
	return fmt.Sprintf("/api/v1/files/thumbnail?key=%s&w=%d", url.QueryEscape(objectKey), width)
}
