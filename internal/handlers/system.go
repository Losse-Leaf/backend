package handlers

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// GetSystemStatus 获取系统冷启动与初始化状态探针 (公开)
func GetSystemStatus(c *gin.Context) {
	var cfg models.SiteConfig
	db.DB.First(&cfg)

	hasMasterKey := strings.TrimSpace(config.AppConfig.AdminAPIKey) != ""

	Success(c, gin.H{
		"initialized":               cfg.Initialized,
		"site_name":                 cfg.SiteName,
		"curator_name":              cfg.CuratorName,
		"has_master_key_configured": hasMasterKey,
	})
}

type BootstrapRequest struct {
	CuratorName    string `json:"curator_name" binding:"required"`
	Location       string `json:"location"`
	MasterPasscode string `json:"master_passcode"`
	MasterKey      string `json:"master_key"`
}

// BootstrapSystem 初次装帧创建自留地 (冷启动，仅在未初始化时允许调用一次)
func BootstrapSystem(c *gin.Context) {
	var cfg models.SiteConfig
	db.DB.First(&cfg)

	if cfg.Initialized {
		Error(c, http.StatusForbidden, "自留地已完成初次装帧，禁止二次重复初始化")
		return
	}

	var req BootstrapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少必要的主理人初始信息")
		return
	}

	// 若服务器环境配置了 ADMIN_API_KEY，核验传入的 master_key 防抢注
	expectedMasterKey := strings.TrimSpace(config.AppConfig.AdminAPIKey)
	if expectedMasterKey != "" {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.MasterKey)), []byte(expectedMasterKey)) != 1 {
			Error(c, http.StatusUnauthorized, "Master Key 校验不通过，无法执行根装帧")
			return
		}
	}

	curatorName := strings.TrimSpace(req.CuratorName)
	location := strings.TrimSpace(req.Location)

	// 1. 初始化 SiteConfig
	cfg.CuratorName = curatorName
	cfg.DefaultLocation = location
	cfg.Initialized = true
	if req.MasterPasscode != "" {
		cfg.MasterPasscode = HashToken(req.MasterPasscode)
	}
	db.DB.Save(&cfg)

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
	db.DB.Save(&profile)

	// 3. 颁发根管理凭证并注册信任设备
	rootToken := fmt.Sprintf("lltk_root_%s", generateRandomHex(16))
	tokenHash := HashToken(rootToken)

	device := models.Device{
		DeviceID:     fmt.Sprintf("dev_root_%s", generateRandomHex(4)),
		Name:         "初次装帧根控制台",
		Platform:     "web",
		TokenHash:    tokenHash,
		Status:       "active",
		PairedAt:     time.Now(),
		LastActiveAt: time.Now(),
	}
	db.DB.Create(&device)

	Success(c, gin.H{
		"token":        rootToken,
		"curator_name": curatorName,
		"location":     location,
	})
}
