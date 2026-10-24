package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// CalculateElapsedInstallmentMonths 根据购买时间(occurredAt)与每月还款日(repaymentDay)，计算截至 now 已经历的还款期数与剩余期数
func CalculateElapsedInstallmentMonths(occurredAt time.Time, totalMonths int, repaymentDay int, now time.Time) (paidMonths int, remainMonths int) {
	if totalMonths <= 0 {
		totalMonths = 12
	}
	if repaymentDay < 1 {
		repaymentDay = 10
	}
	if repaymentDay > 31 {
		repaymentDay = 31
	}
	if occurredAt.IsZero() {
		return 0, totalMonths
	}

	// 全项目时区基准为中国北京时间 (RC-134)，还款日切分不随部署机时区漂移
	loc := config.BeijingLocation
	occInLoc := occurredAt.In(loc)
	occY, occM, occD := occInLoc.Date()
	purchaseDate := time.Date(occY, occM, occD, 0, 0, 0, 0, loc)

	nowY, nowM, nowD := now.Date()
	todayDate := time.Date(nowY, nowM, nowD, 0, 0, 0, 0, loc)

	if !todayDate.After(purchaseDate) {
		return 0, totalMonths
	}

	y, m := occY, occM
	for paidMonths < totalMonths {
		daysInMonth := time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day()
		day := repaymentDay
		if day > daysInMonth {
			day = daysInMonth
		}
		repDate := time.Date(y, m, day, 0, 0, 0, 0, loc)
		if repDate.After(todayDate) {
			break
		}
		if repDate.After(purchaseDate) {
			paidMonths++
		}
		m++
		if m > 12 {
			m = 1
			y++
		}
	}

	if paidMonths > totalMonths {
		paidMonths = totalMonths
	}
	remainMonths = totalMonths - paidMonths
	return paidMonths, remainMonths
}

// InstallmentPeriodAmount 返回第 period 期（1 起）应还金额
//
// 关键（DEC-02 口径 A）：**前 N-1 期取整除，最后一期吃掉尾差**。
// 于是「各期之和 == 总额」永远成立，账单对得上：
//
//	base = total / months
//	rem  = total % months
//	第 1..N-1 期各 base
//	第 N 期 base + rem
//
// 这样「月供 × 期数 ≠ 总额」的矛盾就不存在了。
func InstallmentPeriodAmount(totalAmount int64, totalMonths int, monthlyPay int64, period int) int64 {
	if totalAmount <= 0 || totalMonths <= 0 {
		return 0
	}
	if monthlyPay <= 0 {
		monthlyPay = totalAmount / int64(totalMonths)
	}
	if period < 1 {
		period = 1
	}
	if period > totalMonths {
		period = totalMonths
	}
	remainder := totalAmount % int64(totalMonths)
	if period == totalMonths {
		// 最后一期吃掉尾差
		return monthlyPay + remainder
	}
	return monthlyPay
}

// CalculateInstallmentAmounts 精确计算分期已还金额与剩余待还本金（消除整除尾差，保证 paidAmount + remainingDebt == totalAmount）
func CalculateInstallmentAmounts(totalAmount int64, totalMonths int, monthlyPay int64, paidMonths int, remainMonths int) (paidAmount int64, remainingDebt int64) {
	if totalAmount <= 0 {
		return 0, 0
	}
	if totalMonths <= 0 {
		totalMonths = 12
	}
	if monthlyPay <= 0 {
		monthlyPay = totalAmount / int64(totalMonths)
	}
	if remainMonths <= 0 || paidMonths >= totalMonths {
		return totalAmount, 0
	}
	if paidMonths <= 0 || remainMonths >= totalMonths {
		return 0, totalAmount
	}
	paidAmount = int64(paidMonths) * monthlyPay
	if paidAmount > totalAmount {
		paidAmount = totalAmount
	}
	remainingDebt = totalAmount - paidAmount
	return paidAmount, remainingDebt
}

// AssembleInstallmentTransaction 组装分期账单核心字段 (根据购买时间 OccurredAt 与渠道还款日自动推算剩余期数)
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

	rDay := repaymentDay
	var ch models.CreditChannel
	if err := dbTx.Where("code = ?", chCode).First(&ch).Error; err == nil && ch.RepaymentDay > 0 {
		if rDay <= 0 {
			rDay = ch.RepaymentDay
		}
	}
	if rDay <= 0 {
		rDay = 10
	}
	tx.RepaymentDay = rDay

	// 根据购买时间 (tx.OccurredAt) 与每月还款日自动核算剩余期数
	_, autoRemain := CalculateElapsedInstallmentMonths(tx.OccurredAt, months, rDay, time.Now())
	if remainMonths <= 0 || remainMonths == months || remainMonths > autoRemain {
		tx.InstallmentRemainMonths = autoRemain
	} else {
		tx.InstallmentRemainMonths = remainMonths
	}
}

// CreditChannelRequest 信贷渠道新增与修改入参 (RC-53)
type CreditChannelRequest struct {
	Code string `json:"code"`
	Name string `json:"name" binding:"required"`
	// MOB-P1-35：省略即默认 10 号
	RepaymentDay int    `json:"repayment_day"` // 1~31
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

	// MOB-P1-35：省略还款日（0）时回落到默认 10 号
	repaymentDay := req.RepaymentDay
	if repaymentDay == 0 {
		repaymentDay = 10
	}
	if repaymentDay < 1 || repaymentDay > 31 {
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
		RepaymentDay: repaymentDay,
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
	if err := db.DB.First(&ch, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "信贷渠道不存在")
		return
	}

	var req CreditChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	oldRepaymentDay := ch.RepaymentDay
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
		targetCode := ch.Code
		if codeChanged {
			// 同步迁移既有分期账目的渠道归属，杜绝旧 Transaction 渠道归属断裂
			if err := tx.Model(&models.Transaction{}).
				Where("installment_channel_code = ?", oldCode).
				Update("installment_channel_code", newCode).Error; err != nil {
				return err
			}
			targetCode = newCode
		}
		// 只有还款日真的变化时才重算剩余期数。
		// 用户可能手工下调过剩余期数（提前还款），改个渠道名字不该把它覆盖掉。
		var relatedTxs []models.Transaction
		if ch.RepaymentDay != oldRepaymentDay {
			if err := tx.Where("is_installment = ? AND installment_channel_code = ?", true, targetCode).Find(&relatedTxs).Error; err == nil {
				now := time.Now()
				for _, rtx := range relatedTxs {
					_, autoRemain := CalculateElapsedInstallmentMonths(rtx.OccurredAt, rtx.InstallmentMonths, ch.RepaymentDay, now)
					rtx.RepaymentDay = ch.RepaymentDay
					rtx.InstallmentRemainMonths = autoRemain
					_ = tx.Save(&rtx).Error
				}
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
	if err := db.DB.First(&ch, "id = ?", id).Error; err != nil {
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
