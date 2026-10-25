package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/authclaim"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// claimCodeManager 是进程内唯一的一次性认领码状态。
// 它不落库、不写配置文件，服务重启后由 main 重新生成并打印。
var claimCodeManager = authclaim.NewManager()

// errClaimEntryClosed 表示认领入口已经关闭（系统已完成首次认领）。
var errClaimEntryClosed = errors.New("claim entry closed")

// StartClaimCode 生成一枚新的认领码，供 main 在启动时打印。
func StartClaimCode() (string, time.Time, error) {
	return claimCodeManager.Generate()
}

// ClaimDevice 使用启动时打印的一次性认领码认领首台设备。
//
// POST /api/v1/auth/claim
//
// 仅当 initialized=false 时可用；成功后在同一事务中创建设备记录并把
// initialized 置为 true，随后永久关闭入口（返回 404 隐藏路由）。
func ClaimDevice(c *gin.Context) {
	var cfg models.SiteConfig
	if err := db.DB.Limit(1).Find(&cfg).Error; err != nil {
		Error(c, http.StatusInternalServerError, "读取系统初始化状态失败")
		return
	}
	if cfg.ID == 0 {
		if err := db.DB.FirstOrCreate(&cfg).Error; err != nil {
			Error(c, http.StatusInternalServerError, "初始化站点配置失败")
			return
		}
	}
	if cfg.Initialized {
		claimEntryClosed(c)
		return
	}

	var req struct {
		Code        string `json:"code" binding:"required"`
		DeviceName  string `json:"device_name"`
		Platform    string `json:"platform"`
		DeviceModel string `json:"device_model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请输入启动日志中的认领码")
		return
	}

	inputCode := strings.TrimSpace(req.Code)
	if inputCode == "" {
		Error(c, http.StatusBadRequest, "请输入启动日志中的认领码")
		return
	}

	rotated, err := claimCodeManager.Verify(inputCode)
	switch {
	case err == nil:
		// 认领码已消费，继续创建设备。
	case errors.Is(err, authclaim.ErrTooManyFailures):
		printRotatedClaimCode(c, rotated)
		Error(c, http.StatusTooManyRequests, "认领码连续输错 5 次，已作废并生成新码，请查看服务器启动日志")
		return
	case errors.Is(err, authclaim.ErrExpired), errors.Is(err, authclaim.ErrNoActiveCode):
		Error(c, http.StatusGone, "认领码已过期，请重启服务获取新的认领码")
		return
	case errors.Is(err, authclaim.ErrInvalid):
		Error(c, http.StatusUnauthorized, "认领码不正确，请核对启动日志中的 8 位认领码")
		return
	default:
		Error(c, http.StatusInternalServerError, "核验认领码失败: "+err.Error())
		return
	}

	now := time.Now()
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		platform = "web"
	}
	deviceName := strings.TrimSpace(req.DeviceName)
	if deviceName == "" {
		deviceName = "Web 浏览器 · 首次认领"
	}

	rawDeviceToken := fmt.Sprintf("lldt_%s", generateRandomHex(16))
	device := models.Device{
		DeviceID:     fmt.Sprintf("dev_%s_%s_%s", platform, now.Format("20060102"), generateRandomHex(2)),
		Name:         deviceName,
		Platform:     platform,
		DeviceModel:  strings.TrimSpace(req.DeviceModel),
		TokenHash:    middleware.HashToken(rawDeviceToken),
		Status:       "active",
		PairedAt:     now,
		LastActiveAt: now,
	}

	// 条件更新 initialized=false -> true，防止并发双认领；
	// 设备创建与状态翻转在同一事务里，任一失败都整体回滚。
	err = db.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.SiteConfig{}).
			Where("id = ? AND initialized = ?", cfg.ID, false).
			Updates(map[string]interface{}{"initialized": true, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errClaimEntryClosed
		}
		return tx.Create(&device).Error
	})

	if err != nil {
		if errors.Is(err, errClaimEntryClosed) {
			claimEntryClosed(c)
			return
		}
		// 事务失败时旧码已经被消费，重新生成并打印，避免主人被锁在门外。
		newCode, _, genErr := StartClaimCode()
		if genErr != nil {
			Error(c, http.StatusInternalServerError, "保存首台设备失败，且生成新认领码失败: "+genErr.Error())
			return
		}
		printRotatedClaimCode(c, newCode)
		Error(c, http.StatusInternalServerError, "保存首台设备失败，已生成新认领码，请查看服务器启动日志后重试")
		return
	}

	// MOB-P1-69：认领成功的浏览器直接拿到 HttpOnly 会话 Cookie，
	// 令牌不必再由前端写入 localStorage。
	middleware.SetSessionCookie(c, rawDeviceToken)

	c.JSON(http.StatusOK, Response{
		Code:    http.StatusOK,
		Message: "device claimed successfully",
		Data: gin.H{
			"device_id":    device.DeviceID,
			"device_name":  device.Name,
			"platform":     device.Platform,
			"paired_at":    device.PairedAt.Format(time.RFC3339),
			"device_token": rawDeviceToken,
			"initialized":  true,
		},
	})
}

// claimEntryClosed 统一返回 404，避免向外界暴露「这里曾经有认领入口」。
func claimEntryClosed(c *gin.Context) {
	Error(c, http.StatusNotFound, "未找到该接口")
}

// printRotatedClaimCode 在认领码轮换时把新码打印到标准输出。
func printRotatedClaimCode(c *gin.Context, code string) {
	if strings.TrimSpace(code) == "" {
		return
	}
	fmt.Printf("认领码已更新。请打开 %s/ 并输入认领码：%s（15 分钟内有效）\n", resolveHubURL(c), authclaim.Format(code))
}
