package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"chimu-lab/internal/config"
	"chimu-lab/internal/db"
	"chimu-lab/internal/models"

	"github.com/gin-gonic/gin"
	qrcode "github.com/skip2/go-qrcode"
)

// HashToken 计算 Token 的 SHA-256 哈希字符串，数据库仅存储哈希
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

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

// generateQRCodeDataURL 生成 base64 PNG Data URL 二维码 (RG-02)
func generateQRCodeDataURL(content string) string {
	pngBytes, err := qrcode.Encode(content, qrcode.Medium, 256)
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

// InitPairing 签发 5 分钟有效期的配对 Ticket 与 6 位 PIN 码 (需管理员/已配对凭据)
func InitPairing(c *gin.Context) {
	var req InitPairingRequest
	_ = c.ShouldBindJSON(&req)

	ticketStr := fmt.Sprintf("pair_tk_%s", generateRandomHex(8))
	pinCode := generatePinCode()
	expiresIn := 300 // 5 分钟
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	ticket := models.PairTicket{
		Ticket:    ticketStr,
		PinCode:   pinCode,
		ExpiresAt: expiresAt,
		Used:      false,
	}

	if err := db.DB.Create(&ticket).Error; err != nil {
		Error(c, http.StatusInternalServerError, "生成配对工单失败: "+err.Error())
		return
	}

	hubURL := strings.TrimSpace(req.HubURL)
	if hubURL == "" {
		hubURL = resolveHubURL(c)
	}
	hubURL = strings.TrimRight(hubURL, "/")

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
// 2. 确认设备配对握手 (Confirm Pairing)
// ==========================================

type ConfirmPairingRequest struct {
	Ticket      string `json:"ticket"`
	PinCode     string `json:"pin_code"`
	DeviceName  string `json:"device_name"`
	Platform    string `json:"platform"`
	DeviceModel string `json:"device_model"`
}

// ConfirmPairing 新设备提交 Ticket 或 PIN 码完成握手，获取永久 Device Token
func ConfirmPairing(c *gin.Context) {
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

	var ticket models.PairTicket
	query := db.DB.Where("used = ? AND expires_at > ?", false, time.Now())
	if ticketVal != "" {
		query = query.Where("ticket = ?", ticketVal)
	} else {
		query = query.Where("pin_code = ?", pinVal)
	}

	if err := query.First(&ticket).Error; err != nil {
		Error(c, http.StatusBadRequest, "配对工单无效、已过期或已被核销，请重新发起配对")
		return
	}

	// 核销该工单，杜绝重放攻击
	now := time.Now()
	ticket.Used = true
	ticket.UsedAt = &now
	db.DB.Save(&ticket)

	// 生成设备独立凭证与唯一 ID
	rawDeviceToken := fmt.Sprintf("lldt_live_%s", generateRandomHex(16))
	tokenHash := HashToken(rawDeviceToken)

	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		platform = "device"
	}
	deviceID := fmt.Sprintf("dev_%s_%s", platform, generateRandomHex(4))

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

	if err := db.DB.Create(&device).Error; err != nil {
		Error(c, http.StatusInternalServerError, "保存配对设备记录失败: "+err.Error())
		return
	}

	hubURL := resolveHubURL(c)

	Success(c, gin.H{
		"device_id":    device.DeviceID,
		"device_name":  device.Name,
		"device_token": rawDeviceToken,
		"hub_url":      hubURL,
	})
}

// ==========================================
// 3. 设备列表与吊销管理
// ==========================================

// GetTrustedDevices 获取已配对信任的设备列表
func GetTrustedDevices(c *gin.Context) {
	var devices []models.Device
	if err := db.DB.Order("status ASC, last_active_at DESC, id DESC").Find(&devices).Error; err != nil {
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

	// 标记为 revoked 状态或直接彻底移除
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
// 4. 网页端反向扫码登录与口令验证 (Reverse QR & Passcode)
// ==========================================

type CreateQRRequest struct {
	HubURL string `json:"hub_url"`
}

// CreateQRSession 申请网页端反向登录二维码 Session (公开)
func CreateQRSession(c *gin.Context) {
	var req CreateQRRequest
	_ = c.ShouldBindJSON(&req)

	sessionID := fmt.Sprintf("qr_sess_%s", generateRandomHex(8))
	expiresIn := 120 // 2分钟
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	sess := models.QRSession{
		SessionID: sessionID,
		Status:    "pending",
		ExpiresAt: expiresAt,
	}

	if err := db.DB.Create(&sess).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建扫码会话失败: "+err.Error())
		return
	}

	hubURL := strings.TrimSpace(req.HubURL)
	if hubURL == "" {
		hubURL = resolveHubURL(c)
	}
	hubURL = strings.TrimRight(hubURL, "/")

	qrPayload := fmt.Sprintf("looseleaf://login?hub=%s&session_id=%s", hubURL, sessionID)
	qrDataURL := generateQRCodeDataURL(qrPayload)

	Success(c, gin.H{
		"session_id":         sessionID,
		"hub_url":            hubURL,
		"qr_payload":         qrPayload,
		"qr_code_data_url":   qrDataURL,
		"expires_in_seconds": expiresIn,
	})
}

type AuthorizeQRRequest struct {
	SessionID string `json:"session_id" binding:"required"`
}

// AuthorizeQRSession 手机 App 确认授权网页登录 (需已配对受信任设备或 Master Key 凭据)
func AuthorizeQRSession(c *gin.Context) {
	var req AuthorizeQRRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少 session_id")
		return
	}

	var sess models.QRSession
	if err := db.DB.Where("session_id = ? AND expires_at > ?", req.SessionID, time.Now()).First(&sess).Error; err != nil {
		Error(c, http.StatusBadRequest, "二维码会话无效或已过期，请刷新网页重试")
		return
	}

	if sess.Status == "authorized" {
		Success(c, gin.H{"message": "会话已处于授权状态"})
		return
	}

	// 为该网页会话签发独立管理 Token
	webToken := fmt.Sprintf("lltk_web_%s", generateRandomHex(16))
	tokenHash := HashToken(webToken)

	// 获取授权操作设备信息
	authorDeviceName := "受信任移动设备"
	if devName, exists := c.Get("device_name"); exists {
		if s, ok := devName.(string); ok && s != "" {
			authorDeviceName = s
		}
	}

	device := models.Device{
		DeviceID:     fmt.Sprintf("dev_web_%s", generateRandomHex(4)),
		Name:         fmt.Sprintf("Web 浏览器 (由 %s 扫码授权)", authorDeviceName),
		Platform:     "web",
		TokenHash:    tokenHash,
		Status:       "active",
		PairedAt:     time.Now(),
		LastActiveAt: time.Now(),
	}
	db.DB.Create(&device)

	sess.Status = "authorized"
	sess.Token = webToken
	db.DB.Save(&sess)

	Success(c, gin.H{
		"session_id": sess.SessionID,
		"status":     "authorized",
		"message":    "网页端已成功授权登录",
	})
}

// PollQRSession 网页端轮询反向扫码登录结果 (公开)
func PollQRSession(c *gin.Context) {
	sessionID := c.Param("session_id")
	var sess models.QRSession

	if err := db.DB.Where("session_id = ?", sessionID).First(&sess).Error; err != nil {
		Success(c, gin.H{"status": "expired"})
		return
	}

	if time.Now().After(sess.ExpiresAt) {
		Success(c, gin.H{"status": "expired"})
		return
	}

	if sess.Status == "authorized" {
		Success(c, gin.H{
			"status": "authorized",
			"token":  sess.Token,
		})
		return
	}

	Success(c, gin.H{
		"status": "pending",
	})
}

type VerifyPasscodeRequest struct {
	Passcode string `json:"passcode" binding:"required"`
}

// VerifyPasscode 主理人口令直接登入 (公开，验证成功颁发管理 Token)
func VerifyPasscode(c *gin.Context) {
	var req VerifyPasscodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请输入口令")
		return
	}

	inputPasscode := strings.TrimSpace(req.Passcode)
	if inputPasscode == "" {
		Error(c, http.StatusBadRequest, "口令不能为空")
		return
	}

	// 1. 比对 Master Key
	masterKey := strings.TrimSpace(config.AppConfig.AdminAPIKey)
	matched := false
	if masterKey != "" && subtle.ConstantTimeCompare([]byte(inputPasscode), []byte(masterKey)) == 1 {
		matched = true
	}

	// 2. 比对 SiteConfig 中的 MasterPasscode
	if !matched {
		var cfg models.SiteConfig
		if err := db.DB.First(&cfg).Error; err == nil && cfg.MasterPasscode != "" {
			inputHash := HashToken(inputPasscode)
			if subtle.ConstantTimeCompare([]byte(inputHash), []byte(cfg.MasterPasscode)) == 1 {
				matched = true
			}
		}
	}

	// 3. 本地无密开发模式
	if !matched && masterKey == "" {
		matched = true
	}

	if !matched {
		Error(c, http.StatusUnauthorized, "口令不正确，请重新输入")
		return
	}

	// 签发管理 Token 并写入 Device
	loginToken := fmt.Sprintf("lltk_passcode_%s", generateRandomHex(16))
	tokenHash := HashToken(loginToken)

	device := models.Device{
		DeviceID:     fmt.Sprintf("dev_web_%s", generateRandomHex(4)),
		Name:         "Web 控制台 (口令登录)",
		Platform:     "web",
		TokenHash:    tokenHash,
		Status:       "active",
		PairedAt:     time.Now(),
		LastActiveAt: time.Now(),
	}
	db.DB.Create(&device)

	Success(c, gin.H{
		"token":   loginToken,
		"message": "口令验证通过，已授权进入暗房管理中枢",
	})
}
