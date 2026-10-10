package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// GetSystemStatus 获取系统冷启动与初始化状态探针 (公开 BE-18, BE-19)
func GetSystemStatus(c *gin.Context) {
	var cfg models.SiteConfig
	db.DB.Limit(1).Find(&cfg)

	siteName := cfg.SiteName
	if siteName == "" {
		siteName = "散页 · 时光手账与生活档案"
	}

	Success(c, gin.H{
		"initialized":      cfg.Initialized,
		"has_passcode_set": strings.TrimSpace(cfg.MasterPasscode) != "",
		"site_name":        siteName,
		"curator_name":     cfg.CuratorName,
	})
}

type BootstrapRequest struct {
	CuratorName     string `json:"curator_name" binding:"required"`
	Location        string `json:"location"`
	MasterPasscode  string `json:"master_passcode"`
	ConfirmPasscode string `json:"confirm_passcode"`
	HubURL          string `json:"hub_url"`
}

// BootstrapSystem 发起首次装帧：双次确认密钥并签发首台设备配对工单 (BE-18, BE-19)
// 注意：本接口仅保存主理人信息与密钥 SHA-256 哈希，保持 initialized = false；
// 需待首台设备调用 POST /api/v1/auth/pair/confirm 完成握手后，才将 initialized 置为 true。
func BootstrapSystem(c *gin.Context) {
	var cfg models.SiteConfig
	db.DB.Limit(1).Find(&cfg)

	if cfg.Initialized {
		Error(c, http.StatusForbidden, "自留地已完成初次装帧，禁止二次重复初始化")
		return
	}

	var req BootstrapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少必要的主理人初始信息")
		return
	}

	masterPasscode := strings.TrimSpace(req.MasterPasscode)
	confirmPasscode := strings.TrimSpace(req.ConfirmPasscode)
	if masterPasscode == "" {
		Error(c, http.StatusBadRequest, "请设定管理员密钥")
		return
	}
	if masterPasscode != confirmPasscode {
		Error(c, http.StatusBadRequest, "两次输入的管理员密钥不一致")
		return
	}

	curatorName := strings.TrimSpace(req.CuratorName)
	location := strings.TrimSpace(req.Location)

	// 1. 保存 SiteConfig (此时保持 Initialized = false，等待首台设备配对激活 BE-19)
	if cfg.ID == 0 {
		db.DB.FirstOrCreate(&cfg)
	}
	if cfg.SiteName == "" {
		cfg.SiteName = "散页 · 时光手账与生活档案"
	}
	cfg.CuratorName = curatorName
	cfg.DefaultLocation = location
	cfg.MasterPasscode = HashToken(masterPasscode)
	cfg.Initialized = false
	cfg.UpdatedAt = time.Now()
	if err := db.DB.Save(&cfg).Error; err != nil {
		Error(c, http.StatusInternalServerError, "保存站点配置失败: "+err.Error())
		return
	}

	// 2. 初始化 CuratorProfile (未填写的字段保持纯净空字符串或空数组 RC-52)
	var profile models.CuratorProfile
	db.DB.FirstOrCreate(&profile)
	profile.Name = curatorName
	profile.Location = location
	profile.CurrentCity = location
	if strings.TrimSpace(profile.SocialLinks) == "" {
		profile.SocialLinks = "{}"
	}
	if strings.TrimSpace(profile.EDCGear) == "" {
		profile.EDCGear = "[]"
	}
	profile.UpdatedAt = time.Now()
	db.DB.Save(&profile)

	// 3. 结合前端上报的 hub_url 签发 5 分钟有效的首台设备配对工单 (BE-19, BE-20)
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
		"curator_name":       curatorName,
		"location":           location,
		"ticket":             ticketStr,
		"pin_code":           pinCode,
		"hub_url":            hubURL,
		"qr_payload":         qrPayload,
		"qr_code_data_url":   qrDataURL,
		"expires_at":         expiresAt.Format(time.RFC3339),
		"expires_in_seconds": expiresIn,
	})
}
