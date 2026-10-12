package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AssembleInstallmentTransaction 组装分期账单核心字段 (渠道默认 huabei、期数默认 12、还款日查渠道/默认 10)
func AssembleInstallmentTransaction(
	tx *models.Transaction,
	isInstallment bool,
	channelCode string,
	totalAmount int64,
	fallbackTotal int64,
	months int,
	monthlyPay int64,
	remainMonths int,
	repaymentDay int,
	dbTx *gorm.DB,
) {
	if !isInstallment || tx == nil {
		return
	}
	if dbTx == nil {
		dbTx = db.DB
	}

	tx.IsInstallment = true
	chCode := strings.TrimSpace(channelCode)
	if chCode == "" {
		chCode = "huabei"
	}
	tx.InstallmentChannelCode = chCode

	total := totalAmount
	if total <= 0 {
		total = fallbackTotal
	}
	tx.InstallmentTotalAmount = total

	if months <= 0 {
		months = 12
	}
	tx.InstallmentMonths = months

	mPay := monthlyPay
	if mPay <= 0 && months > 0 {
		mPay = total / int64(months)
	}
	tx.InstallmentMonthlyPay = mPay

	rMonths := remainMonths
	if rMonths <= 0 {
		rMonths = months
	}
	tx.InstallmentRemainMonths = rMonths

	rDay := repaymentDay
	if rDay <= 0 {
		var ch models.CreditChannel
		if err := dbTx.Where("code = ?", chCode).First(&ch).Error; err == nil && ch.RepaymentDay > 0 {
			rDay = ch.RepaymentDay
		} else {
			rDay = 10
		}
	}
	tx.RepaymentDay = rDay
}

// CreditChannelRequest 信贷渠道新增与修改入参 (RC-53)
type CreditChannelRequest struct {
	Code         string `json:"code"`
	Name         string `json:"name" binding:"required"`
	RepaymentDay int    `json:"repayment_day" binding:"required"` // 1~31
	Color        string `json:"color"`
}

// GetCreditChannels 获取所有信贷分期渠道配置列表 (RC-53)
func GetCreditChannels(c *gin.Context) {
	var channels []models.CreditChannel
	if err := db.DB.Order("id ASC").Find(&channels).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取信贷渠道失败: "+err.Error())
		return
	}
	Success(c, channels)
}

// CreateCreditChannel 新建自定义信贷分期渠道 (RC-53)
func CreateCreditChannel(c *gin.Context) {
	var req CreditChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "渠道名称与每月还款日必填")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		Error(c, http.StatusBadRequest, "渠道名称不能为空")
		return
	}

	if req.RepaymentDay < 1 || req.RepaymentDay > 31 {
		Error(c, http.StatusBadRequest, "每月还款日必须在 1 至 31 日之间")
		return
	}

	code := strings.TrimSpace(req.Code)
	if code == "" {
		code = fmt.Sprintf("custom_%d", time.Now().UnixNano())
	}

	// 检查 code 是否已存在
	var count int64
	db.DB.Model(&models.CreditChannel{}).Where("code = ?", code).Count(&count)
	if count > 0 {
		Error(c, http.StatusConflict, "信贷渠道代码已存在")
		return
	}

	color := strings.TrimSpace(req.Color)
	if color == "" {
		color = "var(--accent-warm)"
	}

	ch := models.CreditChannel{
		Code:         code,
		Name:         name,
		RepaymentDay: req.RepaymentDay,
		IsBuiltin:    false,
		Color:        color,
	}

	if err := db.DB.Create(&ch).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建信贷渠道失败: "+err.Error())
		return
	}

	Success(c, ch)
}

// UpdateCreditChannel 修改信贷分期渠道配置
func UpdateCreditChannel(c *gin.Context) {
	id := c.Param("id")
	var ch models.CreditChannel
	if err := db.DB.First(&ch, id).Error; err != nil {
		Error(c, http.StatusNotFound, "信贷渠道不存在")
		return
	}

	var req CreditChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	if strings.TrimSpace(req.Name) != "" {
		ch.Name = strings.TrimSpace(req.Name)
	}
	if req.RepaymentDay >= 1 && req.RepaymentDay <= 31 {
		ch.RepaymentDay = req.RepaymentDay
	} else if req.RepaymentDay != 0 {
		Error(c, http.StatusBadRequest, "每月还款日必须在 1 至 31 日之间")
		return
	}
	if strings.TrimSpace(req.Color) != "" {
		ch.Color = strings.TrimSpace(req.Color)
	}

	oldCode := ch.Code
	codeChanged := false
	newCode := ""
	// 如果是非内置渠道，允许更新 code（若传入且不重复）
	if !ch.IsBuiltin && strings.TrimSpace(req.Code) != "" && strings.TrimSpace(req.Code) != ch.Code {
		newCode = strings.TrimSpace(req.Code)
		var count int64
		db.DB.Model(&models.CreditChannel{}).Where("code = ? AND id != ?", newCode, ch.ID).Count(&count)
		if count > 0 {
			Error(c, http.StatusConflict, "信贷渠道代码已被占用")
			return
		}
		ch.Code = newCode
		codeChanged = true
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&ch).Error; err != nil {
			return err
		}
		if codeChanged {
			// 同步迁移既有分期账目的渠道归属，杜绝旧 Transaction 渠道归属断裂
			if err := tx.Model(&models.Transaction{}).
				Where("installment_channel_code = ?", oldCode).
				Update("installment_channel_code", newCode).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "更新信贷渠道失败: "+err.Error())
		return
	}

	Success(c, ch)
}

// DeleteCreditChannel 删除自定义信贷分期渠道 (内置渠道不可删除)
func DeleteCreditChannel(c *gin.Context) {
	id := c.Param("id")
	var ch models.CreditChannel
	if err := db.DB.First(&ch, id).Error; err != nil {
		Error(c, http.StatusNotFound, "信贷渠道不存在")
		return
	}

	if ch.IsBuiltin {
		Error(c, http.StatusBadRequest, "系统内置信贷渠道不可删除")
		return
	}

	if err := db.DB.Delete(&ch).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除信贷渠道失败: "+err.Error())
		return
	}

	Success(c, gin.H{"id": ch.ID, "deleted": true})
}
