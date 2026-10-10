package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"chimu-lab/internal/config"
	"chimu-lab/internal/db"
	"chimu-lab/internal/models"

	"github.com/gin-gonic/gin"
)

// hashToken 计算 Token 的 SHA-256 哈希
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AdminAuthRequired 管理员操作鉴权中间件 (支持母密钥与已配对设备双轨鉴权)
// 轨一：匹配服务器 .env 中的 ADMIN_API_KEY (Master Key)
// 轨二：匹配 devices 表中有效激活的 Device Token (受信任设备)
func AdminAuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		expectedMasterKey := strings.TrimSpace(config.AppConfig.AdminAPIKey)

		// 1. 提取客户端携带的 Token (按优先级检索各标准头)
		token := ""

		// (1) Authorization: Bearer <token>
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		}

		// (2) X-Device-Token: <token>
		if token == "" {
			token = strings.TrimSpace(c.GetHeader("X-Device-Token"))
		}

		// (3) X-Admin-Key: <token>
		if token == "" {
			token = strings.TrimSpace(c.GetHeader("X-Admin-Key"))
		}

		// (4) Query 参数备用
		if token == "" {
			token = strings.TrimSpace(c.Query("admin_key"))
		}

		// 2. 轨一校验：比对母密钥 (Master Key) - 采用常数时间比较防范侧信道攻击
		if token != "" && expectedMasterKey != "" {
			if subtle.ConstantTimeCompare([]byte(token), []byte(expectedMasterKey)) == 1 {
				c.Set("auth_type", "master")
				c.Next()
				return
			}
		}

		// 3. 轨二校验：比对已配对受信任设备令牌 (Device Token)
		if token != "" && db.DB != nil {
			tHash := hashToken(token)
			var device models.Device
			if err := db.DB.Where("token_hash = ? AND status = 'active'", tHash).First(&device).Error; err == nil {
				// 异步轻量更新设备的最近活跃时间 (带 60 秒节流防抖 BE-03)
				now := time.Now()
				if now.Sub(device.LastActiveAt) > time.Minute {
					go func(dID uint, activeTime time.Time) {
						if db.DB != nil {
							_ = db.DB.Model(&models.Device{}).Where("id = ?", dID).Update("last_active_at", activeTime).Error
						}
					}(device.ID, now)
				}

				c.Set("auth_type", "device")
				c.Set("device_id", device.DeviceID)
				c.Set("device_name", device.Name)
				c.Next()
				return
			}
		}

		// 4. 若服务器未配置母密钥，且处于本地无密开发模式，放行体验
		if expectedMasterKey == "" {
			c.Set("auth_type", "dev_open")
			c.Next()
			return
		}

		// 5. 校验均未通过，拒绝访问
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code":    http.StatusUnauthorized,
			"message": "未授权的写操作：无效的管理员密钥或设备凭证。请在右上角设置中核对，或通过配对扫码接入。",
			"data":    nil,
		})
	}
}
