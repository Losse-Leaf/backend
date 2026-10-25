package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// HashToken 计算 Token 的 SHA-256 哈希
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SessionCookieName 会话 Cookie 名 (MOB-P1-69)
// 浏览器端用它承载设备子钥匙，从而让令牌彻底不落在 localStorage / 可被脚本读取的位置。
const SessionCookieName = "looseleaf_session"

// sessionCookieMaxAge 会话 Cookie 有效期 (30 天)
const sessionCookieMaxAge = 30 * 24 * 3600

// ExtractToken 提取客户端携带的设备凭证 Token (支持 Authorization: Bearer, X-Device-Token、X-Admin-Key 请求头与 Cookie)
// 安全规范：严禁通过 URL Query 参数传递 Token，防止凭据泄露至访问日志、反代日志与 Referer
//
// MOB-P1-69：Cookie 作为**兜底**通道，优先级排在三个请求头之后 ——
// 这样现有 Android / macOS 客户端（都用请求头）的行为一字不变，
// 而浏览器端可以把令牌放进 HttpOnly Cookie（JS 读不到 ⇒ 一次 XSS 也偷不走）。
func ExtractToken(c *gin.Context) string {
	authHeader := c.GetHeader("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	}
	if t := strings.TrimSpace(c.GetHeader("X-Device-Token")); t != "" {
		return t
	}
	if t := strings.TrimSpace(c.GetHeader("X-Admin-Key")); t != "" {
		return t
	}
	// MOB-P1-69：Cookie 兜底
	if ck, err := c.Cookie(SessionCookieName); err == nil {
		if t := strings.TrimSpace(ck); t != "" {
			return t
		}
	}
	return ""
}

// SetSessionCookie 下发 HttpOnly 会话 Cookie (MOB-P1-69)
//
// Secure 由实际传输协议决定（直连 TLS 或反代透传 X-Forwarded-Proto）——
// 局域网纯 HTTP 部署时若硬置 Secure=true，浏览器会直接拒收，登录反而失效。
func SetSessionCookie(c *gin.Context, token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   sessionCookieMaxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie 清除会话 Cookie (登出, MOB-P1-69)
func ClearSessionCookie(c *gin.Context) {
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// IsAdmin 判定当前请求是否具备主理人权限 (仅認 devices 表中 status = 'active' 的有效设备凭证 BE-18)
func IsAdmin(c *gin.Context) bool {
	if authType, exists := c.Get("auth_type"); exists && authType == "device" {
		return true
	}

	token := ExtractToken(c)
	if token == "" || db.DB == nil {
		return false
	}

	tHash := HashToken(token)
	var count int64
	if err := db.DB.Model(&models.Device{}).Where("token_hash = ? AND status = 'active'", tHash).Count(&count).Error; err == nil && count > 0 {
		return true
	}

	return false
}

// AdminAuthRequired 管理员写操作与私密接口鉴权中间件 (BE-18)
// 唯一凭证源：校验 SQLite devices 表中处于 active 状态的设备子钥匙 (token_hash)
// 未初始化时不存在任何免密旁路；首台设备只能通过公开的 /api/v1/auth/claim 认领。
func AdminAuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ExtractToken(c)

		if token != "" && db.DB != nil {
			tHash := HashToken(token)
			var device models.Device
			if err := db.DB.Where("token_hash = ? AND status = 'active'", tHash).Limit(1).Find(&device).Error; err == nil && device.ID > 0 {
				// 最近活跃时间在请求内同步更新（60 秒节流 BE-03）。
				// 不能放进 goroutine：它可能活过整库恢复，把旧设备写进新库，
				// 或是撞上正在关闭的连接池。
				now := time.Now()
				if now.Sub(device.LastActiveAt) > time.Minute {
					_ = db.DB.Model(&models.Device{}).Where("id = ?", device.ID).Update("last_active_at", now).Error
				}

				c.Set("auth_type", "device")
				c.Set("device_id", device.DeviceID)
				c.Set("device_name", device.Name)
				c.Next()
				return
			}
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code":    http.StatusUnauthorized,
			"message": "未授权的操作：缺少有效的访问凭证",
			"data":    nil,
		})
	}
}
