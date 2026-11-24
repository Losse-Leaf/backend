package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// generateRandomHex 生成 n 字节随机十六进制串
func generateRandomHex(n int) string {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

// resolveHubURL 解析中枢外部访问基址（优先采用显式 SITE_URL，其次根据安全传输与转发头推断）。
func resolveHubURL(c *gin.Context) string {
	if siteURL := strings.TrimRight(strings.TrimSpace(config.AppConfig.SiteURL), "/"); siteURL != "" {
		return siteURL
	}
	scheme := "http"
	if middleware.IsSecureRequest(c) {
		scheme = "https"
	}
	var host string
	if c != nil && c.Request != nil {
		if xfHost := strings.TrimSpace(c.GetHeader("X-Forwarded-Host")); xfHost != "" {
			host = strings.TrimSpace(strings.Split(xfHost, ",")[0])
		} else {
			host = strings.TrimSpace(c.Request.Host)
		}
	}
	if host == "" {
		host = "localhost:8088"
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

// oidcAccountInfo OIDC 账号基础身份快照（写入会话记录并回传给客户端展示）
type oidcAccountInfo struct {
	Sub   string `json:"sub,omitempty"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

func (a oidcAccountInfo) DisplayLabel() string {
	if s := strings.TrimSpace(a.Name); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.Email); s != "" {
		return s
	}
	return strings.TrimSpace(a.Sub)
}

// mintTrustedDevice 统一签发会话实体与一次性明文访问令牌。
func mintTrustedDevice(platform, defaultName, rawName, rawModel string, now time.Time) (models.Device, string) {
	return mintAccountSession(platform, defaultName, rawName, rawModel, oidcAccountInfo{}, now)
}

// mintAccountSession 签发携带 OAuth2/OIDC 账号信息的客户端登录会话与访问令牌。
func mintAccountSession(platform, defaultName, rawName, rawModel string, acct oidcAccountInfo, now time.Time) (models.Device, string) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "" {
		platform = "device"
	}
	if now.IsZero() {
		now = time.Now()
	}
	deviceName := strings.TrimSpace(rawName)
	if deviceName == "" {
		deviceName = defaultName
	}
	if deviceName == "" {
		switch platform {
		case "android":
			deviceName = "Android 客户端"
		case "macos":
			deviceName = "macOS 桌面端"
		case "web":
			deviceName = "Web 浏览器"
		default:
			deviceName = fmt.Sprintf("%s 客户端", strings.ToUpper(platform))
		}
	}

	rawDeviceToken := fmt.Sprintf("lldt_%s", generateRandomHex(16))
	device := models.Device{
		DeviceID:     fmt.Sprintf("dev_%s_%s_%s", platform, now.Format("20060102"), generateRandomHex(4)),
		Name:         deviceName,
		Platform:     platform,
		DeviceModel:  strings.TrimSpace(rawModel),
		AccountSub:   strings.TrimSpace(acct.Sub),
		AccountName:  strings.TrimSpace(acct.Name),
		AccountEmail: strings.TrimSpace(acct.Email),
		TokenHash:    middleware.HashToken(rawDeviceToken),
		Status:       "active",
		PairedAt:     now,
		LastActiveAt: now,
	}
	return device, rawDeviceToken
}

// issueAuthExchangeCode 在指定连接或事务内签发一枚指定 TTL 的原生客户端一次性 OAuth2 回调兑换码。
func issueAuthExchangeCode(tx *gorm.DB, hubURL string, ttl time.Duration, acct oidcAccountInfo) (models.PairTicket, error) {
	if tx == nil {
		tx = db.DB
	}
	codeRec := models.PairTicket{
		Ticket:       fmt.Sprintf("auth_code_%s", generateRandomHex(16)),
		HubURL:       hubURL,
		AccountSub:   strings.TrimSpace(acct.Sub),
		AccountName:  strings.TrimSpace(acct.Name),
		AccountEmail: strings.TrimSpace(acct.Email),
		ExpiresAt:    time.Now().Add(ttl),
		Used:         false,
	}
	if err := tx.Create(&codeRec).Error; err != nil {
		return codeRec, err
	}
	return codeRec, nil
}

// buildAuthCallbackDeepLink 构建标准 OAuth2 原生客户端回跳深链 (looseleaf://auth/callback?hub=...&code=...)。
func buildAuthCallbackDeepLink(hubURL, code string) string {
	q := url.Values{}
	q.Set("hub", hubURL)
	q.Set("code", code)
	return "looseleaf://auth/callback?" + q.Encode()
}
