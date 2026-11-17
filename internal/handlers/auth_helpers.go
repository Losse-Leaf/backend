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

// mintTrustedDevice 统一签发受信任设备实体与一次性明文设备子钥匙。
func mintTrustedDevice(platform, defaultName, rawName, rawModel string, now time.Time) (models.Device, string) {
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
		deviceName = fmt.Sprintf("受信任的 %s 设备", strings.ToUpper(platform))
	}

	rawDeviceToken := fmt.Sprintf("lldt_%s", generateRandomHex(16))
	device := models.Device{
		DeviceID:     fmt.Sprintf("dev_%s_%s_%s", platform, now.Format("20060102"), generateRandomHex(4)),
		Name:         deviceName,
		Platform:     platform,
		DeviceModel:  strings.TrimSpace(rawModel),
		TokenHash:    middleware.HashToken(rawDeviceToken),
		Status:       "active",
		PairedAt:     now,
		LastActiveAt: now,
	}
	return device, rawDeviceToken
}

// issuePairTicket 在指定连接或事务内签发一枚指定 TTL 的一次性配对工单。
func issuePairTicket(tx *gorm.DB, hubURL string, ttl time.Duration) (models.PairTicket, error) {
	if tx == nil {
		tx = db.DB
	}
	pairTicket := models.PairTicket{
		Ticket:    fmt.Sprintf("pair_tk_%s", generateRandomHex(12)),
		HubURL:    hubURL,
		ExpiresAt: time.Now().Add(ttl),
		Used:      false,
	}
	if err := tx.Create(&pairTicket).Error; err != nil {
		return pairTicket, err
	}
	return pairTicket, nil
}

// buildPairDeepLink 使用标准 URL 查询编码构建跨端配对深链。
func buildPairDeepLink(hubURL, ticket string) string {
	q := url.Values{}
	q.Set("hub", hubURL)
	q.Set("ticket", ticket)
	return "looseleaf://pair?" + q.Encode()
}
