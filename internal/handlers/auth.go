package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	qrcode "github.com/skip2/go-qrcode"
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

func resolveHubURL(c *gin.Context) string {
	scheme := "https"
	if c.Request.TLS == nil {
		scheme = "http"
	}
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := c.Request.Host
	if host == "" {
		host = "localhost:8088"
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

// generateQRCodeDataURL 生成散页纸墨风 base64 PNG Data URL 二维码 (RG-02, BE-20)
// 前景墨色: 深暖墨黑 #181716 (R:24, G:23, B:22)
// 背景纸色: 米白暖调棉纸 #F9F8F5 (R:249, G:248, B:245)
func generateQRCodeDataURL(content string) string {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return ""
	}
	q.ForegroundColor = color.RGBA{R: 24, G: 23, B: 22, A: 255}
	q.BackgroundColor = color.RGBA{R: 249, G: 248, B: 245, A: 255}
	pngBytes, err := q.PNG(256)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
}

// ==========================================
// 1. 发起设备配对工单 (Init Pairing)
// ==========================================

type InitPairingRequest struct {
	HubURL     string `json:"hub_url"`
	DeviceHint string `json:"device_hint"`
}

// InitPairing 签发 5 分钟有效期的配对工单（智能避开 localhost 回环，优先选用 Tailscale / 局域网 Wi-Fi IP）
func InitPairing(c *gin.Context) {
	var req InitPairingRequest
	_ = c.ShouldBindJSON(&req)

	smartHubURL, candidates := ResolveSmartHubURL(req.HubURL, c)

	ticketStr := fmt.Sprintf("pair_tk_%s", generateRandomHex(6))
	expiresIn := 300 // 5 分钟
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	ticket := models.PairTicket{
		Ticket:    ticketStr,
		PinCode:   "",
		HubURL:    smartHubURL,
		ExpiresAt: expiresAt,
		Used:      false,
	}

	if err := db.DB.Create(&ticket).Error; err != nil {
		Error(c, http.StatusInternalServerError, "生成配对工单失败: "+err.Error())
		return
	}

	qrPayload := fmt.Sprintf("looseleaf://pair?hub=%s&ticket=%s", smartHubURL, ticketStr)
	qrDataURL := generateQRCodeDataURL(qrPayload)

	Success(c, gin.H{
		"ticket":             ticketStr,
		"hub_url":            smartHubURL,
		"expires_at":         expiresAt.Format(time.RFC3339),
		"expires_in_seconds": expiresIn,
		"qr_payload":         qrPayload,
		"qr_code_data_url":   qrDataURL,
		"candidates":         candidates,
	})
}

// RenderPairQRCode 动态生成指定中枢节点与工单的纸墨二维码 (GET /api/v1/auth/pair/qr)
func RenderPairQRCode(c *gin.Context) {
	hubURL := strings.TrimRight(strings.TrimSpace(c.Query("hub")), "/")
	ticket := strings.TrimSpace(c.Query("ticket"))
	if hubURL == "" || ticket == "" {
		Error(c, http.StatusBadRequest, "缺少必要参数 hub 或 ticket")
		return
	}

	payload := fmt.Sprintf("looseleaf://pair?hub=%s&ticket=%s", hubURL, ticket)
	dataURL := generateQRCodeDataURL(payload)

	Success(c, gin.H{
		"hub_url":          hubURL,
		"ticket":           ticket,
		"qr_payload":       payload,
		"qr_code_data_url": dataURL,
	})
}

// ==========================================
// 2. 确认设备配对握手并激活系统 (Confirm Pairing & Activate BE-19)
// ==========================================

type ConfirmPairingRequest struct {
	Ticket      string `json:"ticket"`
	DeviceName  string `json:"device_name"`
	Platform    string `json:"platform"`
	DeviceModel string `json:"device_model"`
}

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
	now := time.Now()
	cutoff := now.Add(-window)
	times := l.attempts[ip]
	valid := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	// 窗口内已无有效尝试时回收该 IP 的键，避免 map 随不同 IP 无界增长 (BE-33)
	if len(valid) == 0 {
		delete(l.attempts, ip)
		if len(valid) >= maxAttempts {
			return false
		}
		l.attempts[ip] = append(valid, now)
		return true
	}
	if len(valid) >= maxAttempts {
		l.attempts[ip] = valid
		return false
	}
	valid = append(valid, now)
	l.attempts[ip] = valid
	return true
}

// errPairTicketRedeemed 并发窗口内配对工单已被另一请求抢先核销（双花）
var errPairTicketRedeemed = errors.New("配对工单已被核销")

// errLastActiveDevice 最后一台 active 设备不可吊销（防全端锁死）
var errLastActiveDevice = errors.New("这是最后一台受信任设备，请先配对一台新设备再吊销最后一台")

// ConfirmPairing 新设备通过扫码或配对深链提交 Ticket 完成握手，获取永久 Device Token；
// 若当前系统尚未初始化 (initialized == false)，在同一事务中将 initialized 置为 true (BE-19)
func ConfirmPairing(c *gin.Context) {
	if !pairLimiter.checkRateLimit(c.ClientIP(), 30, time.Minute) {
		Error(c, http.StatusTooManyRequests, "配对请求过于频繁，请稍后再试")
		return
	}

	var req ConfirmPairingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数不合法")
		return
	}

	ticketVal := strings.TrimSpace(req.Ticket)
	if ticketVal == "" {
		Error(c, http.StatusBadRequest, "必须提供配对工单 ticket 进行握手验证")
		return
	}

	// 首台设备必须先通过一次性认领码认领；未初始化时配对入口保持关闭。
	var cfg models.SiteConfig
	if err := db.DB.Limit(1).Find(&cfg).Error; err != nil {
		Error(c, http.StatusInternalServerError, "读取系统初始化状态失败")
		return
	}
	if cfg.ID == 0 || !cfg.Initialized {
		Error(c, http.StatusForbidden, "系统尚未完成首次认领，请先使用启动时打印的认领码认领首台设备")
		return
	}

	now := time.Now()
	var ticket models.PairTicket

	if err := db.DB.Where("ticket = ?", ticketVal).First(&ticket).Error; err != nil {
		Error(c, http.StatusBadRequest, "配对工单无效、已过期或已被核销，请重新发起配对")
		return
	}
	if ticket.Used || now.After(ticket.ExpiresAt) {
		Error(c, http.StatusBadRequest, "配对工单无效、已过期或已被核销，请重新发起配对")
		return
	}

	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		platform = "device"
	}
	dateTag := now.Format("20060102")
	deviceID := fmt.Sprintf("dev_%s_%s_%s", platform, dateTag, generateRandomHex(2))
	rawDeviceToken := fmt.Sprintf("lldt_%s", generateRandomHex(16))
	tokenHash := middleware.HashToken(rawDeviceToken)

	deviceName := strings.TrimSpace(req.DeviceName)
	if deviceName == "" {
		deviceName = fmt.Sprintf("受信任的 %s 设备", strings.ToUpper(platform))
	}

	device := models.Device{
		DeviceID:     deviceID,
		Name:         deviceName,
		Platform:     platform,
		DeviceModel:  strings.TrimSpace(req.DeviceModel),
		TokenHash:    tokenHash,
		Status:       "active",
		PairedAt:     now,
		LastActiveAt: now,
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		// 事务内**条件核销**：UPDATE pair_tickets SET used=1, ... WHERE ticket=? AND used=0。
		// 原先「事务外读 ticket.Used 再无核销条件地 Save」存在 check-then-act 并发窗口，
		// 两台设备可各持同一工单同时握手成功（工单双花）。这里以受影响行数做原子裁决：
		// 抢不到核销权（RowsAffected != 1）的请求整体回滚，不创建设备。
		res := tx.Model(&models.PairTicket{}).
			Where("ticket = ? AND used = ?", ticketVal, false).
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
		Error(c, http.StatusInternalServerError, "保存配对设备记录失败: "+err.Error())
		return
	}

	hubURL := ticket.HubURL
	if hubURL == "" {
		hubURL = resolveHubURL(c)
	}

	// MOB-P1-69：配对成功的浏览器直接拿到 HttpOnly 会话 Cookie，
	// 令牌不必再由前端写入 localStorage（JS 读不到 ⇒ 一次 XSS 也偷不走）。
	// 原生端（Android / macOS）用请求头带令牌，Cookie 对它们无影响。
	middleware.SetSessionCookie(c, rawDeviceToken)

	c.JSON(http.StatusOK, Response{
		Code:    http.StatusOK,
		Message: "device paired successfully",
		Data: gin.H{
			"device_id":    device.DeviceID,
			"device_name":  device.Name,
			"platform":     device.Platform,
			"paired_at":    device.PairedAt.Format(time.RFC3339),
			"device_token": rawDeviceToken,
			"initialized":  true,
			"hub_url":      hubURL,
		},
	})
}

// ==========================================
// 3. 轮询配对工单与装帧激活状态 (Poll Pairing Status BE-19)
// ==========================================

// PollPairingStatus 公开轮询配对工单核销状态与系统初始化状态
// GET /api/v1/auth/pair/status?ticket=pair_tk_...
func PollPairingStatus(c *gin.Context) {
	ticketVal := strings.TrimSpace(c.Query("ticket"))
	if ticketVal == "" {
		Error(c, http.StatusBadRequest, "缺少 ticket 参数")
		return
	}

	var cfg models.SiteConfig
	db.DB.Limit(1).Find(&cfg)

	var ticket models.PairTicket
	if err := db.DB.Where("ticket = ?", ticketVal).Limit(1).Find(&ticket).Error; err != nil || ticket.ID == 0 {
		Success(c, gin.H{
			"status":      "expired",
			"initialized": cfg.Initialized,
		})
		return
	}

	if ticket.Used {
		var devInfo any
		if ticket.PairedDeviceID != "" {
			var pairedDev models.Device
			if err := db.DB.Where("device_id = ?", ticket.PairedDeviceID).Limit(1).Find(&pairedDev).Error; err == nil && pairedDev.ID > 0 {
				devInfo = gin.H{
					"device_id": pairedDev.DeviceID,
					"name":      pairedDev.Name,
					"platform":  pairedDev.Platform,
					"paired_at": pairedDev.PairedAt.Format(time.RFC3339),
				}
			}
		}

		Success(c, gin.H{
			"status":      "paired",
			"initialized": cfg.Initialized,
			"device":      devInfo,
		})
		return
	}

	if time.Now().After(ticket.ExpiresAt) {
		Success(c, gin.H{
			"status":      "expired",
			"initialized": cfg.Initialized,
		})
		return
	}

	Success(c, gin.H{
		"status":      "pending",
		"initialized": cfg.Initialized,
	})
}

// ==========================================
// 4. 设备列表与吊销管理
// ==========================================

// GetTrustedDevices 获取已配对信任的设备列表（仅返回当前有效授信的设备）
func GetTrustedDevices(c *gin.Context) {
	var devices []models.Device
	if err := db.DB.Where("status = ?", "active").Order("last_active_at DESC, id DESC").Find(&devices).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取设备列表失败: "+err.Error())
		return
	}
	Success(c, devices)
}

// RevokeDevice 吊销并注销指定设备
func RevokeDevice(c *gin.Context) {
	id := c.Param("id")
	var device models.Device

	// 支持通过数字主键 ID 或通过 device_id 字符查找
	query := db.DB.Where("id = ? OR device_id = ?", id, id)
	if err := query.First(&device).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该设备或已被删除")
		return
	}

	// 关键：吊销的是最后一台 active 设备时必须拒绝 —— 否则系统里再无任何有效凭据，
	// 主人将被**永久锁死**在门外（启动打印的一次性认领码早已消费、入口永久关闭）。
	// 吊销非最后一台（包括当前请求所用的设备）仍允许：换机后主动吊销旧机是正常操作。
	// 锁死后的自救通道：可由服务器 shell 以 `--reopen-claim` 启动标志重开认领入口
	//（见 main.go：业务数据全部保留并重印一次性认领码）；除此之外**不提供任何网络旁路**。
	//
	// 关键（并发，第三轮 P1）：拒绝裁决必须在**同一写事务**内完成「计数 + 改写」——
	// 原先在事务外先 Count 再 Save 是 check-then-act：仅剩两台 active 时，
	// 两台并发吊销都能通过「count > 1」检查后各自写入 revoked，把 active 设备清空。
	// SQLite 单写者保证下面这条条件 UPDATE 的子查询计数与改写原子；
	// 目标已是 revoked 时跳过（幂等放行，重复吊销不报错）。
	if device.Status == "active" {
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
			// 没抢到改写权：要么只剩这一台（拒绝），要么刚好被并发请求吊销（幂等成功）
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
			Error(c, http.StatusInternalServerError, "吊销设备失败: "+err.Error())
			return
		}
	}

	Success(c, gin.H{
		"id":        device.ID,
		"device_id": device.DeviceID,
		"status":    "revoked",
		"message":   "设备已成功吊销，该设备的凭证已立即失效",
	})
}

// UpdateDeviceName 修改已授信设备的备注名称及设备平台 (PUT /api/v1/auth/devices/:id)
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
		Error(c, http.StatusNotFound, "未找到该活动设备")
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
		Error(c, http.StatusInternalServerError, "更新设备信息失败: "+err.Error())
		return
	}

	Success(c, device)
}

// ==========================================
// 4. 会话探针与登出 (MOB-P1-69)
// ==========================================

// GetSessionStatus 探测当前请求是否具备主理人权限
//
// GET /api/v1/auth/session
//
// 浏览器端把令牌放进 HttpOnly Cookie 后，JS 读不到令牌本身，
// 但仍需要知道「当前是不是主人」来决定是否渲染私密 UI。
// 这个公开探针只回传角色，不回传任何凭据。
func GetSessionStatus(c *gin.Context) {
	isAdmin := middleware.IsAdmin(c)
	role := "guest"
	if isAdmin {
		role = "admin"
	}
	Success(c, gin.H{
		"is_admin": isAdmin,
		"role":     role,
	})
}

// LogoutSession 登出：清除会话 Cookie
//
// POST /api/v1/auth/logout
//
// 只清浏览器侧的 Cookie，不吊销设备记录 ——
// 设备吊销是「主人显式操作」，那是另一条既有接口（DELETE /auth/devices/:id）。
func LogoutSession(c *gin.Context) {
	middleware.ClearSessionCookie(c)
	Success(c, gin.H{"logged_out": true})
}
