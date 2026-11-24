package handlers

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ==========================================
// 1. OAuth2 / OIDC 原生客户端一次性授权码换取会话 (ExchangeOIDCCode)
// ==========================================

type ExchangeOIDCCodeRequest struct {
	Code        string `json:"code"`
	Ticket      string `json:"ticket"` // 兼容旧版客户端字段
	DeviceName  string `json:"device_name"`
	Platform    string `json:"platform"`
	DeviceModel string `json:"device_model"`
}

// ConfirmPairingRequest 兼容旧版客户端与回归测试类型别名
type ConfirmPairingRequest = ExchangeOIDCCodeRequest

type ipAttemptTracker struct {
	sync.Mutex
	attempts map[string][]time.Time
}

var pairLimiter = &ipAttemptTracker{
	attempts: make(map[string][]time.Time),
}

func (l *ipAttemptTracker) checkRateLimit(ip string, maxAttempts int, window time.Duration) bool {
	l.Lock()
	defer l.Unlock()
	if maxAttempts <= 0 {
		return false
	}
	now := time.Now()
	cutoff := now.Add(-window)
	// 容量超过阈值时顺带清扫所有 IP 的过期记录，避免大量一次性 IP 导致 map 无界增长 (BE-33)
	if len(l.attempts) > 256 {
		for k, ts := range l.attempts {
			kept := ts[:0]
			for _, t := range ts {
				if t.After(cutoff) {
					kept = append(kept, t)
				}
			}
			if len(kept) == 0 {
				delete(l.attempts, k)
			} else {
				l.attempts[k] = kept
			}
		}
	}
	times := l.attempts[ip]
	valid := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= maxAttempts {
		l.attempts[ip] = valid
		return false
	}
	l.attempts[ip] = append(valid, now)
	return true
}

// errPairTicketRedeemed 并发窗口内授权码已被另一请求抢先核销（双花）
var errPairTicketRedeemed = errors.New("授权码已被核销")

// errLastActiveDevice 离线认领模式下最后一台 active 设备不可吊销（防全端锁死）
var errLastActiveDevice = errors.New("这是最后一台受信任设备，请先配对一台新设备再吊销最后一台")

// ExchangeOIDCCode 原生客户端（Android / macOS）通过 OAuth2 回调获得的一次性短效授权码换取登录会话。
// POST /api/v1/auth/oidc/exchange（同时兼容旧路由 POST /api/v1/auth/pair/confirm）
func ExchangeOIDCCode(c *gin.Context) {
	if !pairLimiter.checkRateLimit(c.ClientIP(), 30, time.Minute) {
		Error(c, http.StatusTooManyRequests, "登录请求过于频繁，请稍后再试")
		return
	}

	var req ExchangeOIDCCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数不合法")
		return
	}

	codeVal := strings.TrimSpace(req.Code)
	if codeVal == "" {
		codeVal = strings.TrimSpace(req.Ticket)
	}
	if codeVal == "" {
		Error(c, http.StatusBadRequest, "缺少登录授权码 code")
		return
	}

	var cfg models.SiteConfig
	if err := db.DB.Limit(1).Find(&cfg).Error; err != nil {
		Error(c, http.StatusInternalServerError, "读取系统初始化状态失败")
		return
	}
	if cfg.ID == 0 || !cfg.Initialized {
		Error(c, http.StatusForbidden, "系统尚未完成初始化，请先通过 Vesper Auth 登录激活")
		return
	}

	now := time.Now()
	var ticket models.PairTicket

	if err := db.DB.Where("ticket = ?", codeVal).First(&ticket).Error; err != nil {
		Error(c, http.StatusBadRequest, "登录授权码无效、已过期或已被核销，请重新登录")
		return
	}
	if ticket.Used || now.After(ticket.ExpiresAt) {
		Error(c, http.StatusBadRequest, "登录授权码无效、已过期或已被核销，请重新登录")
		return
	}

	acct := oidcAccountInfo{
		Sub:   ticket.AccountSub,
		Name:  ticket.AccountName,
		Email: ticket.AccountEmail,
	}
	device, rawDeviceToken := mintAccountSession(req.Platform, "", req.DeviceName, req.DeviceModel, acct, now)

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		// 事务内条件核销：UPDATE pair_tickets SET used=1, ... WHERE ticket=? AND used=0，
		// 彻底杜绝并发双花。
		res := tx.Model(&models.PairTicket{}).
			Where("ticket = ? AND used = ?", codeVal, false).
			Updates(map[string]any{
				"used":             true,
				"used_at":          now,
				"paired_device_id": device.DeviceID,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errPairTicketRedeemed
		}

		return tx.Create(&device).Error
	})

	if err != nil {
		if errors.Is(err, errPairTicketRedeemed) {
			Error(c, http.StatusBadRequest, errPairTicketRedeemed.Error())
			return
		}
		Error(c, http.StatusInternalServerError, "保存登录会话失败: "+err.Error())
		return
	}

	hubURL := ticket.HubURL
	if hubURL == "" {
		hubURL = resolveHubURL(c)
	}

	middleware.SetSessionCookie(c, rawDeviceToken)

	c.JSON(http.StatusOK, Response{
		Code:    http.StatusOK,
		Message: "login session established",
		Data: gin.H{
			"access_token": rawDeviceToken,
			"device_token": rawDeviceToken, // 兼容旧版客户端字段名
			"session_id":   device.DeviceID,
			"device_id":    device.DeviceID,
			"device_name":  device.Name,
			"platform":     device.Platform,
			"paired_at":    device.PairedAt.Format(time.RFC3339),
			"initialized":  true,
			"hub_url":      hubURL,
			"account": gin.H{
				"sub":   device.AccountSub,
				"name":  device.AccountName,
				"email": device.AccountEmail,
			},
		},
	})
}

// ConfirmPairing 兼容旧路由别名（滚动升级期间平滑过渡）
func ConfirmPairing(c *gin.Context) {
	ExchangeOIDCCode(c)
}

// ==========================================
// 2. 登录会话管理（兼容受保护接口）
// ==========================================

// GetTrustedDevices 获取当前有效的登录会话列表
func GetTrustedDevices(c *gin.Context) {
	var devices []models.Device
	if err := db.DB.Where("status = ?", "active").Order("last_active_at DESC, id DESC").Find(&devices).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取会话列表失败: "+err.Error())
		return
	}
	currentDeviceID := c.GetString("device_id")
	for i := range devices {
		devices[i].IsCurrent = currentDeviceID != "" && devices[i].DeviceID == currentDeviceID
	}
	Success(c, devices)
}

// RevokeDevice 吊销指定会话
func RevokeDevice(c *gin.Context) {
	id := c.Param("id")
	var device models.Device

	query := db.DB.Where("id = ? OR device_id = ?", id, id)
	if err := query.First(&device).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该登录会话或已被注销")
		return
	}

	if device.Status == "active" {
		if config.AppConfig.OIDC.Enabled {
			// OAuth2/OIDC 模式下用户随时可以通过 Vesper Auth 重新登录，不存在永久锁死风险
			if err := db.DB.Model(&models.Device{}).Where("id = ?", device.ID).Update("status", "revoked").Error; err != nil {
				Error(c, http.StatusInternalServerError, "注销会话失败: "+err.Error())
				return
			}
		} else {
			// 离线认领模式下保护最后一台 active 设备不被清空，防止永久锁死
			err := db.DB.Transaction(func(tx *gorm.DB) error {
				res := tx.Model(&models.Device{}).
					Where("id = ? AND status = ? AND (SELECT COUNT(*) FROM devices AS d WHERE d.status = 'active') > 1", device.ID, "active").
					Update("status", "revoked")
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 1 {
					return nil
				}
				var cur models.Device
				if err := tx.Where("id = ?", device.ID).First(&cur).Error; err != nil {
					return err
				}
				if cur.Status != "active" {
					return nil
				}
				return errLastActiveDevice
			})
			if err != nil {
				if errors.Is(err, errLastActiveDevice) {
					Error(c, http.StatusBadRequest, errLastActiveDevice.Error())
					return
				}
				Error(c, http.StatusInternalServerError, "注销会话失败: "+err.Error())
				return
			}
		}
	}

	Success(c, gin.H{
		"id":        device.ID,
		"device_id": device.DeviceID,
		"status":    "revoked",
		"message":   "会话已成功注销，该凭证已立即失效",
	})
}

// UpdateDeviceName 修改登录会话备注名称及平台属性 (PUT /api/v1/auth/devices/:id)
func UpdateDeviceName(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		Platform    string `json:"platform"`
		DeviceModel string `json:"device_model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "设备名称不能为空")
		return
	}

	newName := strings.TrimSpace(req.Name)
	newPlatform := strings.ToLower(strings.TrimSpace(req.Platform))
	newModel := strings.TrimSpace(req.DeviceModel)

	if newName == "" && newPlatform == "" && newModel == "" {
		Error(c, http.StatusBadRequest, "设备名称不能为空")
		return
	}

	var device models.Device
	if err := db.DB.Where("(id = ? OR device_id = ?) AND status = ?", id, id, "active").First(&device).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该活动会话")
		return
	}

	if newName != "" {
		device.Name = newName
	}
	if newPlatform != "" {
		device.Platform = newPlatform
	}
	if newModel != "" {
		device.DeviceModel = newModel
	}

	if err := db.DB.Save(&device).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新会话信息失败: "+err.Error())
		return
	}

	Success(c, device)
}

// ==========================================
// 3. 会话探针与账号登出
// ==========================================

// GetSessionStatus 探测当前请求是否具备已登录主理人身份，并附带当前账号摘要信息。
// GET /api/v1/auth/session
func GetSessionStatus(c *gin.Context) {
	isAdmin := middleware.IsAdmin(c)
	role := "guest"
	if isAdmin {
		role = "admin"
	}

	data := gin.H{
		"is_admin": isAdmin,
		"role":     role,
	}

	if isAdmin && db.DB != nil {
		token := middleware.ExtractToken(c)
		if token != "" {
			var device models.Device
			tHash := middleware.HashToken(token)
			if err := db.DB.Where("token_hash = ? AND status = 'active'", tHash).Limit(1).Find(&device).Error; err == nil && device.ID > 0 {
				data["session_id"] = device.DeviceID
				data["device_id"] = device.DeviceID
				data["platform"] = device.Platform
				data["account"] = gin.H{
					"sub":   device.AccountSub,
					"name":  device.AccountName,
					"email": device.AccountEmail,
				}
			}
		}
	}

	Success(c, data)
}

// LogoutSession 退出账号登录：清除浏览器 Cookie 并吊销当前会话令牌（同时支持 Cookie 与 Bearer Token）。
// POST /api/v1/auth/logout
func LogoutSession(c *gin.Context) {
	token := middleware.ExtractToken(c)
	middleware.ClearSessionCookie(c)

	if token != "" && db.DB != nil {
		tHash := middleware.HashToken(token)
		if config.AppConfig.OIDC.Enabled {
			_ = db.DB.Model(&models.Device{}).
				Where("token_hash = ? AND status = ?", tHash, "active").
				Update("status", "revoked").Error
		} else {
			// 离线模式下避免单会话退出导致锁死，仅当存在其他活跃会话时同步吊销数据库记录
			_ = db.DB.Model(&models.Device{}).
				Where("token_hash = ? AND status = ? AND (SELECT COUNT(*) FROM devices AS d WHERE d.status = 'active') > 1", tHash, "active").
				Update("status", "revoked").Error
		}
	}

	Success(c, gin.H{"logged_out": true})
}
