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
	// mediaSignatureParamExpires 有效期参数名
	mediaSignatureParamExpires = "exp"
	// mediaSignatureParamSig 签名参数名
	mediaSignatureParamSig = "sig"
)

var (
	mediaSigningMu     sync.RWMutex
	mediaSigningSecret string
)

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

// computeMediaSignature 计算某个对象键在某个时刻的签名
func computeMediaSignature(objectKey string, expiresAt int64) string {
	mac := hmac.New(sha256.New, MediaSigningSecret())
	mac.Write([]byte(objectKey))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.FormatInt(expiresAt, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignLocalMediaURL 生成本机媒体的签名访问 URL
//
// 返回形如 `/media/<key>?exp=1700000000&sig=abcdef...`
func SignLocalMediaURL(objectKey string, customDuration ...time.Duration) string {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return ""
	}
	ttl := mediaSignatureTTL
	if len(customDuration) > 0 && customDuration[0] > 0 {
		ttl = customDuration[0]
	}
	expiresAt := time.Now().Add(ttl).Unix()
	sig := computeMediaSignature(objectKey, expiresAt)
	return fmt.Sprintf("/media/%s?%s=%d&%s=%s",
		objectKey,
		mediaSignatureParamExpires, expiresAt,
		mediaSignatureParamSig, sig)
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
	expected := computeMediaSignature(objectKey, expiresAt)
	return hmac.Equal([]byte(expected), []byte(sigRaw))
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
