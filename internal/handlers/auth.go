package handlers

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image/color"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
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

func generatePinCode() string {
	max := big.NewInt(900000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return fmt.Sprintf("%06d", (time.Now().UnixNano()%900000)+100000)
	}
	return fmt.Sprintf("%06d", n.Int64()+100000)
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

// InitPairing 签发 5 分钟有效期的配对 Ticket 与 6 位 PIN 码 (需已授权设备凭据)
func InitPairing(c *gin.Context) {
	var req InitPairingRequest
	_ = c.ShouldBindJSON(&req)

	hubURL := strings.TrimSpace(req.HubURL)
	if hubURL == "" {
		hubURL = resolveHubURL(c)
	}
	hubURL = strings.TrimRight(hubURL, "/")

	ticketStr := fmt.Sprintf("pair_tk_%s", generateRandomHex(6))
	pinCode := generatePinCode()
	expiresIn := 300 // 5 分钟
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	ticket := models.PairTicket{
		Ticket:    ticketStr,
		PinCode:   pinCode,
		HubURL:    hubURL,
		ExpiresAt: expiresAt,
		Used:      false,
	}

	if err := db.DB.Create(&ticket).Error; err != nil {
		Error(c, http.StatusInternalServerError, "生成配对工单失败: "+err.Error())
		return
	}

	qrPayload := fmt.Sprintf("looseleaf://pair?hub=%s&ticket=%s", hubURL, ticketStr)
	qrDataURL := generateQRCodeDataURL(qrPayload)

	Success(c, gin.H{
		"ticket":             ticketStr,
		"pin_code":           pinCode,
		"hub_url":            hubURL,
		"expires_at":         expiresAt.Format(time.RFC3339),
		"expires_in_seconds": expiresIn,
		"qr_payload":         qrPayload,
		"qr_code_data_url":   qrDataURL,
	})
}

// ==========================================
// 2. 确认设备配对握手并激活系统 (Confirm Pairing & Activate BE-19)
// ==========================================

type ConfirmPairingRequest struct {
	Ticket      string `json:"ticket"`
	PinCode     string `json:"pin_code"`
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

// ConfirmPairing 新设备提交 Ticket 或 PIN 码完成握手，获取永久 Device Token；
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
	pinVal := strings.TrimSpace(req.PinCode)

	if ticketVal == "" && pinVal == "" {
		Error(c, http.StatusBadRequest, "必须提供 ticket 或 pin_code 进行握手验证")
		return
	}

	now := time.Now()
	var ticket models.PairTicket

	if ticketVal != "" {
		if err := db.DB.Where("ticket = ?", ticketVal).First(&ticket).Error; err != nil {
			Error(c, http.StatusBadRequest, "配对工单无效、已过期或已被核销，请重新发起配对")
			return
		}
		if ticket.Used || now.After(ticket.ExpiresAt) {
			Error(c, http.StatusBadRequest, "配对工单无效、已过期或已被核销，请重新发起配对")
			return
		}
		if ticket.FailedAttempts >= 5 {
			Error(c, http.StatusBadRequest, "配对工单尝试次数过多已作废，请重新发起配对")
			return
		}
		if pinVal != "" && subtle.ConstantTimeCompare([]byte(ticket.PinCode), []byte(pinVal)) != 1 {
			ticket.FailedAttempts++
			if ticket.FailedAttempts >= 5 {
				ticket.Used = true
			}
			db.DB.Save(&ticket)
			Error(c, http.StatusBadRequest, "配对 PIN 码错误")
			return
		}
	} else {
		// 纯 PIN 握手防爆破 (最多 5 次错误尝试后作废)
		var activeTickets []models.PairTicket
		db.DB.Where("used = ? AND expires_at > ? AND failed_attempts < 5", false, now).Find(&activeTickets)
		if len(activeTickets) == 0 {
			Error(c, http.StatusBadRequest, "配对工单无效、已过期或已被核销，请重新发起配对")
			return
		}

		var matchedTicket *models.PairTicket
		for i := range activeTickets {
			if subtle.ConstantTimeCompare([]byte(activeTickets[i].PinCode), []byte(pinVal)) == 1 {
				matchedTicket = &activeTickets[i]
				break
			}
		}

		if matchedTicket == nil {
			// 纯 PIN 握手无法定位工单，不做跨工单累加 (BE-26)：
			// 否则 5 次错误 PIN 即可作废系统内全部有效工单，形成拒绝服务。
			// 防爆破由 ConfirmPairing 入口的 IP 级限速 (30 次/分钟) 兜底。
			Error(c, http.StatusBadRequest, "配对 PIN 码错误或工单无效")
			return
		}

		ticket = *matchedTicket
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
		if err := tx.Create(&device).Error; err != nil {
			return err
		}

		var cfg models.SiteConfig
		tx.Limit(1).Find(&cfg)
		if cfg.ID == 0 {
			tx.FirstOrCreate(&cfg)
		}

		webCompanionToken := ""
		// 若系统尚未完成首次装帧激活，在此事务中正式将 initialized 置为 true，并为发起装帧的浏览器签发伴生令牌 (BE-19)
		if !cfg.Initialized {
			if platform != "web" {
				webCompanionToken = fmt.Sprintf("lldt_%s", generateRandomHex(16))
				webDev := models.Device{
					DeviceID:     fmt.Sprintf("dev_web_%s_%s", dateTag, generateRandomHex(2)),
					Name:         fmt.Sprintf("Web 浏览器 · 伴生授权 (%s)", deviceName),
					Platform:     "web",
					TokenHash:    middleware.HashToken(webCompanionToken),
					Status:       "active",
					PairedAt:     now,
					LastActiveAt: now,
				}
				if err := tx.Create(&webDev).Error; err != nil {
					return err
				}
			} else {
				webCompanionToken = rawDeviceToken
			}

			cfg.Initialized = true
			cfg.UpdatedAt = now
			if err := tx.Save(&cfg).Error; err != nil {
				return err
			}
		}

		ticket.Used = true
		ticket.UsedAt = &now
		ticket.PairedDeviceID = device.DeviceID
		ticket.WebToken = webCompanionToken
		return tx.Save(&ticket).Error
	})

	if err != nil {
		Error(c, http.StatusInternalServerError, "保存配对设备记录失败: "+err.Error())
		return
	}

	hubURL := ticket.HubURL
	if hubURL == "" {
		hubURL = resolveHubURL(c)
	}

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

		webToken := ticket.WebToken
		if webToken != "" {
			// 单次读取后立即清除数据库中的伴生明文令牌，防止重放泄露 (BE-19)
			db.DB.Model(&ticket).Update("web_token", "")
		}

		Success(c, gin.H{
			"status":      "paired",
			"initialized": cfg.Initialized,
			"device":      devInfo,
			"web_token":   webToken,
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

	device.Status = "revoked"
	if err := db.DB.Save(&device).Error; err != nil {
		Error(c, http.StatusInternalServerError, "吊销设备失败: "+err.Error())
		return
	}

	Success(c, gin.H{
		"id":        device.ID,
		"device_id": device.DeviceID,
		"status":    "revoked",
		"message":   "设备已成功吊销，该设备的凭证已立即失效",
	})
}

// ==========================================
// 5. 日常欢迎扉页：输入管理员密钥以主人身份进入 (Verify Admin Key BE-18)
// ==========================================

type VerifyPasscodeRequest struct {
	Passcode   string `json:"passcode" binding:"required"`
	DeviceName string `json:"device_name"`
}

// HashPasscode 使用 bcrypt 进行加盐慢哈希加密 (杜绝彩虹表与硬件离线爆破)
func HashPasscode(passcode string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(passcode), bcrypt.DefaultCost)
	return string(bytes), err
}

// CheckPasscode 校验密码：优先匹配 bcrypt，并向下兼容旧有裸 SHA-256 哈希
func CheckPasscode(passcode, hashed string) bool {
	if strings.HasPrefix(hashed, "$2a$") || strings.HasPrefix(hashed, "$2b$") {
		return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(passcode)) == nil
	}
	inputHash := middleware.HashToken(passcode)
	return subtle.ConstantTimeCompare([]byte(inputHash), []byte(hashed)) == 1
}

var verifyPasscodeLimiter = &ipAttemptTracker{
	attempts: make(map[string][]time.Time),
}

// VerifyPasscode 校验 site_configs.master_passcode 的哈希（bcrypt / SHA-256），限速防爆破并签发 Web 设备凭据 (BE-18)
func VerifyPasscode(c *gin.Context) {
	if !verifyPasscodeLimiter.checkRateLimit(c.ClientIP(), 5, time.Minute) {
		Error(c, http.StatusTooManyRequests, "密码尝试过于频繁，请稍后再试")
		return
	}

	var req VerifyPasscodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请输入管理员密钥")
		return
	}

	inputPasscode := strings.TrimSpace(req.Passcode)
	if inputPasscode == "" {
		Error(c, http.StatusBadRequest, "管理员密钥不能为空")
		return
	}

	var cfg models.SiteConfig
	if err := db.DB.Limit(1).Find(&cfg).Error; err != nil || cfg.ID == 0 || strings.TrimSpace(cfg.MasterPasscode) == "" {
		Error(c, http.StatusUnauthorized, "系统尚未完成首次装帧或未设定管理员密钥")
		return
	}

	if !CheckPasscode(inputPasscode, cfg.MasterPasscode) {
		Error(c, http.StatusUnauthorized, "管理员密钥不正确，请重新输入")
		return
	}

	// 渐进式平滑升级：旧有 SHA-256 密码在首次验证成功后自动升级为 bcrypt 加盐慢哈希
	if !strings.HasPrefix(cfg.MasterPasscode, "$2a$") && !strings.HasPrefix(cfg.MasterPasscode, "$2b$") {
		if newBcryptHash, err := HashPasscode(inputPasscode); err == nil {
			cfg.MasterPasscode = newBcryptHash
			db.DB.Model(&cfg).Update("master_passcode", newBcryptHash)
		}
	}

	now := time.Now()
	rawToken := fmt.Sprintf("lldt_%s", generateRandomHex(16))
	tokenHash := middleware.HashToken(rawToken)
	deviceName := strings.TrimSpace(req.DeviceName)
	if deviceName == "" {
		deviceName = "Web 浏览器 · 主人密钥登录"
	}

	// RC-82: 检查是否已存在同平台同名的 Web 登录设备记录，避免同一浏览器重复登录无限累积记录
	var existingDevice models.Device
	if err := db.DB.Where("platform = ? AND name = ?", "web", deviceName).First(&existingDevice).Error; err == nil && existingDevice.ID != 0 {
		existingDevice.TokenHash = tokenHash
		existingDevice.Status = "active"
		existingDevice.LastActiveAt = now
		if err := db.DB.Save(&existingDevice).Error; err != nil {
			Error(c, http.StatusInternalServerError, "更新设备凭证失败: "+err.Error())
			return
		}
		c.JSON(http.StatusOK, Response{
			Code:    http.StatusOK,
			Message: "密钥验证通过，已授权以主人身份进入",
			Data: gin.H{
				"token":       rawToken,
				"device_id":   existingDevice.DeviceID,
				"device_name": existingDevice.Name,
			},
		})
		return
	}

	deviceID := fmt.Sprintf("dev_web_%s_%s", now.Format("20060102"), generateRandomHex(2))
	device := models.Device{
		DeviceID:     deviceID,
		Name:         deviceName,
		Platform:     "web",
		TokenHash:    tokenHash,
		Status:       "active",
		PairedAt:     now,
		LastActiveAt: now,
	}
	if err := db.DB.Create(&device).Error; err != nil {
		Error(c, http.StatusInternalServerError, "签发设备凭证失败: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, Response{
		Code:    http.StatusOK,
		Message: "密钥验证通过，已授权以主人身份进入",
		Data: gin.H{
			"token":       rawToken,
			"device_id":   deviceID,
			"device_name": deviceName,
		},
	})
}
