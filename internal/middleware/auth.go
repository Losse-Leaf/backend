package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// HashToken 计算 Token 的 SHA-256 哈希
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashToken(token string) string {
	return HashToken(token)
}

// ExtractToken 提取客户端携带的 Token (按优先级检索各标准头)
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
	return strings.TrimSpace(c.Query("admin_key"))
}

// isSystemInitialized 检查系统是否已完成初次装帧或设置了主理人口令 (BE-13)
func isSystemInitialized() bool {
	if db.DB == nil {
		return false
	}
	var cfg models.SiteConfig
	if err := db.DB.First(&cfg).Error; err != nil {
		return false
	}
	return cfg.Initialized || strings.TrimSpace(cfg.MasterPasscode) != ""
}

// IsAdmin 判定当前请求是否具备管理员权限 (母密钥或有效激活设备)
func IsAdmin(c *gin.Context) bool {
	if authType, exists := c.Get("auth_type"); exists && authType != "" {
		return true
	}

	token := ExtractToken(c)

	expectedMasterKey := strings.TrimSpace(config.AppConfig.AdminAPIKey)
	if token != "" && expectedMasterKey != "" {
		if subtle.ConstantTimeCompare([]byte(token), []byte(expectedMasterKey)) == 1 {
			return true
		}
	}

	if token != "" && db.DB != nil {
		tHash := HashToken(token)
		var count int64
		if err := db.DB.Model(&models.Device{}).Where("token_hash = ? AND status = 'active'", tHash).Count(&count).Error; err == nil && count > 0 {
			return true
		}
	}

	// 仅当系统未初始化纯空态且未配置母密钥时，允许本地初始引导体验 (BE-13)
	if expectedMasterKey == "" && !isSystemInitialized() {
		return true
	}

	return false
}

// AdminAuthRequired 管理员操作鉴权中间件 (支持母密钥与已配对设备双轨鉴权)
// 轨一：匹配服务器 .env 中的 ADMIN_API_KEY (Master Key)
// 轨二：匹配 devices 表中有效激活的 Device Token (受信任设备)
func AdminAuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		expectedMasterKey := strings.TrimSpace(config.AppConfig.AdminAPIKey)

		// 1. 提取客户端携带的 Token (按优先级检索各标准头)
		token := ExtractToken(c)

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
			tHash := HashToken(token)
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

		// 4. 仅当系统未初始化纯空态且未配置母密钥时，允许本地冷启动引导 (BE-13 核心防线)
		if expectedMasterKey == "" && !isSystemInitialized() {
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
