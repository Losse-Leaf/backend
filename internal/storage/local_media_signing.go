// local_media_signing.go
// 本机媒体的签名 URL
//
// 背景：中枢的真实存储是 VPS 本地磁盘，附件通过 `/media/<object_key>` 提供。
// 早先这个路由是**无鉴权的静态托管**（`r.Static("/media", uploadsDir)`），
// 而对象存储那条路走的是**预签名 URL**（带签名与有效期）。
// 两者口径不一致 —— 于是本机部署下，**任何人只要知道中枢地址就能翻阅全部照片**，
// 且绕开了「访客仅限换取已登记附件签名」这条防签名预言机的规则（BE-21）。
//
// 现在：本机媒体也用**签名 URL**，与对象存储同构：
//   /media/<object_key>?exp=<unix秒>&sig=<HMAC>
// - 签名用中枢自己的密钥算，密钥不入库明文也不外发
// - 有效期与 R2 一致（默认 30 分钟，可配置）
// - 裸路径（无签名）一律拒绝
//
// 这样既保住了「同一台机器直读」的性能收益，又不破坏任何客户端：
// 客户端拿到的仍然是**一个可直接访问的 URL**，只是它带签名。

package storage

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// mediaSignatureTTL 本机媒体签名 URL 的默认有效期
	mediaSignatureTTL = 30 * time.Minute
	// mediaSignatureWindowSeconds 签名过期时间的对齐窗口（秒）
	//
	// 关键：过期时间必须落在这个窗口的边界上，否则同一张图每序列化一次
	// 就会得到不同的 exp/sig，而缩略图响应又声明了 immutable 缓存 —— 缓存永不命中。
	mediaSignatureWindowSeconds int64 = 300
	// mediaSignatureParamExpires 有效期参数名
	mediaSignatureParamExpires = "exp"
	// mediaSignatureParamSig 签名参数名
	mediaSignatureParamSig = "sig"
)

var (
	mediaSigningMu     sync.RWMutex
	mediaSigningSecret string
)

// LocalMediaSignatureTTL 返回本机媒体签名的默认有效期
func LocalMediaSignatureTTL() time.Duration { return mediaSignatureTTL }

// MediaSigningSecret 返回本机媒体签名密钥（不存在则生成一个并缓存）。
//
// 关键：密钥**只存在进程内存里**，不落盘。
// 代价是中枢重启后旧的签名 URL 全部失效 —— 但它们本来就只有 30 分钟有效期，
// 客户端会在下一次同步时拿到新 URL，这个代价可以接受。
// 换来的是：即使数据库文件被拷走，也无法伪造媒体签名。
func MediaSigningSecret() []byte {
	mediaSigningMu.RLock()
	secret := mediaSigningSecret
	mediaSigningMu.RUnlock()
	if secret != "" {
		return []byte(secret)
	}

	mediaSigningMu.Lock()
	defer mediaSigningMu.Unlock()
	if mediaSigningSecret != "" {
		return []byte(mediaSigningSecret)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// 极端情况下退化为进程内派生值（仍然不会落盘）
		mediaSigningSecret = fmt.Sprintf("fallback-%d-%d", os.Getpid(), time.Now().UnixNano())
	} else {
		mediaSigningSecret = hex.EncodeToString(buf)
	}
	return []byte(mediaSigningSecret)
}

// computeMediaSignature 计算某个资源在某个时刻的签名
//
// resource 是资源的「身份串」：
//   - 本机媒体： "media:" + objectKey
//   - 缩略图：   "thumb:" + objectKey + ":" + width
func computeMediaSignature(resource string, expiresAt int64) string {
	mac := hmac.New(sha256.New, MediaSigningSecret())
	mac.Write([]byte(resource))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.FormatInt(expiresAt, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// 本机媒体资源的身份串
func mediaResource(objectKey string) string { return "media:" + objectKey }

// 缩略图资源的身份串
func thumbnailResource(objectKey string, width int) string {
	return fmt.Sprintf("thumb:%s:%d", objectKey, width)
}

// mediaSignatureExpiry 计算签名 URL 的过期时间（unix 秒）。
//
// 过期时间按 300 秒窗口对齐，使**同一窗口内**同一对象生成的 URL 完全一致，
// 从而让 immutable 缓存真正命中。
//
// 实现要点：窗口锚点取「当前窗口的结束时刻」，再叠加 TTL 后向上取整。
// 这样既保证 exp 恒定，又保证 exp >= now+ttl。
// （若直接对 now+ttl 向上取整，当 TTL 恰好是 300 的整数倍时，
// 窗口内第一秒与其余秒会落在不同的 300 秒档位上，URL 仍会变化。）
func mediaSignatureExpiry(now time.Time, ttl time.Duration) int64 {
	ttlSeconds := int64((ttl + time.Second - 1) / time.Second)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	windowEnd := (now.Unix()/mediaSignatureWindowSeconds + 1) * mediaSignatureWindowSeconds
	return ((windowEnd + ttlSeconds + mediaSignatureWindowSeconds - 1) / mediaSignatureWindowSeconds) * mediaSignatureWindowSeconds
}

// signLocalMediaURLAt 在指定时刻签发本机媒体 URL（便于测试窗口对齐）
func signLocalMediaURLAt(objectKey string, ttl time.Duration, now time.Time) string {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return ""
	}
	expiresAt := mediaSignatureExpiry(now, ttl)
	sig := computeMediaSignature(mediaResource(objectKey), expiresAt)
	return fmt.Sprintf("/media/%s?%s=%d&%s=%s",
		objectKey,
		mediaSignatureParamExpires, expiresAt,
		mediaSignatureParamSig, sig)
}

// SignLocalMediaURL 生成本机媒体的签名访问 URL
//
// 返回形如 `/media/<key>?exp=1700000000&sig=abcdef...`
func SignLocalMediaURL(objectKey string, customDuration ...time.Duration) string {
	ttl := mediaSignatureTTL
	if len(customDuration) > 0 && customDuration[0] > 0 {
		ttl = customDuration[0]
	}
	return signLocalMediaURLAt(objectKey, ttl, time.Now())
}

// VerifyLocalMediaSignature 校验本机媒体访问请求
//
// 规则：
//   - 必须带 exp 与 sig
//   - exp 必须是合法 unix 秒且**未过期**
//   - sig 必须与按同一密钥算出的值一致（常数时间比较）
//
// 另外：已配对的设备令牌 / 会话 Cookie 也直接放行 ——
// 主人自己的客户端拿到的 URL 可能没带签名（历史数据），
// 且它本来就有最高权限。
func VerifyLocalMediaSignature(objectKey string, expRaw string, sigRaw string) bool {
	if objectKey == "" {
		return false
	}
	expRaw = strings.TrimSpace(expRaw)
	sigRaw = strings.TrimSpace(sigRaw)
	if expRaw == "" || sigRaw == "" {
		return false
	}
	expiresAt, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > expiresAt {
		return false
	}
	expected := computeMediaSignature(mediaResource(objectKey), expiresAt)
	return hmac.Equal([]byte(expected), []byte(sigRaw))
}

// signLocalThumbnailURLAt 在指定时刻签发缩略图 URL（便于测试窗口对齐）
func signLocalThumbnailURLAt(objectKey string, width int, now time.Time) string {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return ""
	}
	if width <= 0 {
		width = 480
	}
	expiresAt := mediaSignatureExpiry(now, mediaSignatureTTL)
	sig := computeMediaSignature(thumbnailResource(objectKey, width), expiresAt)
	return fmt.Sprintf("/api/v1/files/thumbnail?key=%s&w=%d&%s=%d&%s=%s",
		url.QueryEscape(objectKey), width,
		mediaSignatureParamExpires, expiresAt,
		mediaSignatureParamSig, sig)
}

// SignLocalThumbnailURL 生成缩略图的签名访问 URL
//
// 关键：缩略图路由也必须签名 —— 否则它就是一个公开的「按 key 取图」接口，
// 任何知道 object_key 的人都能拿到你的相纸。
//
// 过期时间的窗口对齐规则与原图一致（见 mediaSignatureExpiry）。
func SignLocalThumbnailURL(objectKey string, width int) string {
	return signLocalThumbnailURLAt(objectKey, width, time.Now())
}

// VerifyLocalThumbnailSignature 校验缩略图访问请求
func VerifyLocalThumbnailSignature(objectKey string, width int, expRaw string, sigRaw string) bool {
	if objectKey == "" {
		return false
	}
	expiresAt, err := strconv.ParseInt(strings.TrimSpace(expRaw), 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > expiresAt {
		return false
	}
	expected := computeMediaSignature(thumbnailResource(objectKey, width), expiresAt)
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(sigRaw)))
}

// LocalMediaPathFromRequest 从请求路径里取出对象键，并做穿越防护
func LocalMediaPathFromRequest(rawPath string) (string, bool) {
	cleaned := strings.TrimPrefix(strings.TrimSpace(rawPath), "/")
	if cleaned == "" {
		return "", false
	}
	// 归一化后必须仍在 uploads 根目录内
	base := GetLocalUploadDir()
	if base == "" {
		base = filepath.Join("data", "uploads")
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", false
	}
	absTarget, err := filepath.Abs(filepath.Join(absBase, filepath.FromSlash(cleaned)))
	if err != nil {
		return "", false
	}
	if absTarget != absBase && !strings.HasPrefix(absTarget, absBase+string(filepath.Separator)) {
		return "", false
	}
	return cleaned, true
}
