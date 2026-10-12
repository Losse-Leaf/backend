package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// ChannelLiabilitySummary 信贷渠道负债聚合摘要 (RC-53)
type ChannelLiabilitySummary struct {
	ChannelCode        string               `json:"channel_code"`
	ChannelName        string               `json:"channel_name"`
	RepaymentDay       int                  `json:"repayment_day"`
	Color              string               `json:"color"`
	TotalDebt          int64                `json:"total_debt"`          // 该渠道剩余待还总额（分）
	MonthlyPay         int64                `json:"monthly_pay"`         // 每月待还总额（分）
	DaysUntilRepayment int                  `json:"days_until_repayment"` // 距离下次还款日天数倒计时
	ActiveCount        int                  `json:"active_count"`        // 活跃分期笔数
	Installments       []models.Transaction `json:"installments"`        // 关联分期账目流水列表
}

// GearSummary 随身与固定资产设备库摘要 (RC-53)
type GearSummary struct {
	TotalCount         int               `json:"total_count"`
	ActiveCount        int               `json:"active_count"`
	ActiveValue        int64             `json:"active_value"`         // 在用固资总价值（分）
	SoldCount          int               `json:"sold_count"`
	SoldRecoveredValue int64             `json:"sold_recovered_value"` // 二手转让回血总额（分）
	Items              []models.GearItem `json:"items"`
}

// AssetsOverviewResponse 资产大盘看板聚合响应 (RC-53, BE-15)
type AssetsOverviewResponse struct {
	NetWorth             int64                      `json:"net_worth"`              // 个人净资产 = 流动可用余额 + 固定资产沉淀 - 待还信贷负债
	LiquidBalance        int64                      `json:"liquid_balance"`         // 流动可用现金余额
	InitialLiquidBalance int64                      `json:"initial_liquid_balance"` // 初始流动现金底数 (BE-15)
	FixedAssets          int64                      `json:"fixed_assets"`           // 在用固定设备资产沉淀
	TotalLiabilities     int64                      `json:"total_liabilities"`      // 待还信贷分期总负债
	MonthlyLiabilities   int64                      `json:"monthly_liabilities"`    // 每月固定还款支出
	GearSummary          GearSummary                `json:"gear_summary"`           // 设备库全生命周期盘点
	ChannelLiabilities   []ChannelLiabilitySummary  `json:"channel_liabilities"`    // 信贷渠道明细与独立还款日倒计时
}

// calculateDaysUntilRepayment 计算距离每月独立还款日的天数倒计时
func calculateDaysUntilRepayment(repaymentDay int) int {
	if repaymentDay < 1 {
		repaymentDay = 1
	}
	if repaymentDay > 31 {
		repaymentDay = 31
	}
	now := time.Now()
	year, month, day := now.Date()

	// 限制当月最大天数（防范二月或小月越界）
	daysInCurrentMonth := time.Date(year, month+1, 0, 0, 0, 0, 0, now.Location()).Day()
	targetDayThisMonth := repaymentDay
	if targetDayThisMonth > daysInCurrentMonth {
		targetDayThisMonth = daysInCurrentMonth
	}

	targetDate := time.Date(year, month, targetDayThisMonth, 0, 0, 0, 0, now.Location())
	todayDate := time.Date(year, month, day, 0, 0, 0, 0, now.Location())

	if day <= targetDayThisMonth {
		diff := int(targetDate.Sub(todayDate).Hours() / 24)
		if diff < 0 {
			diff = 0
		}
		return diff
	}

	// 跨月到次月还款日
	nextMonth := month + 1
	nextYear := year
	if nextMonth > 12 {
		nextMonth = 1
		nextYear++
	}
	daysInNextMonth := time.Date(nextYear, nextMonth+1, 0, 0, 0, 0, 0, now.Location()).Day()
	targetDayNextMonth := repaymentDay
	if targetDayNextMonth > daysInNextMonth {
		targetDayNextMonth = daysInNextMonth
	}
	targetDateNextMonth := time.Date(nextYear, nextMonth, targetDayNextMonth, 0, 0, 0, 0, now.Location())
	diff := int(targetDateNextMonth.Sub(todayDate).Hours() / 24)
	if diff < 0 {
		diff = 0
	}
	return diff
}

func loadProfileGear(profile *models.CuratorProfile) []models.GearItem {
	raw := strings.TrimSpace(profile.EDCGear)
	if raw == "" {
		return []models.GearItem{}
	}
	var list []models.GearItem
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return []models.GearItem{}
	}
	return list
}

func saveProfileGear(profile *models.CuratorProfile, list []models.GearItem) error {
	bytes, err := json.Marshal(list)
	if err != nil {
		return err
	}
	profile.EDCGear = string(bytes)
	profile.UpdatedAt = time.Now()
	return db.DB.Save(profile).Error
}

// GetAssetsOverview 资产管理大盘看板 (RC-53)
// 包含净资产、流动可用余额、在用固资价值、分期总负债与各渠道独立还款日倒计时
func GetAssetsOverview(c *gin.Context) {
	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		Error(c, http.StatusNotFound, "主理人档案尚未初始化")
		return
	}

	gearList := loadProfileGear(&profile)
	activeGearCount := 0
	activeGearValue := int64(0)
	soldGearCount := 0
	soldRecoveredValue := int64(0)

	for _, g := range gearList {
		// 兼容识别后端标准 status="sold" 与前端中文 status="已出二手" (RG-19)
		if g.Status == "sold" || g.Status == "已出二手" {
			soldGearCount++
			soldRecoveredValue += g.SoldPriceCents
		} else {
			activeGearCount++
			activeGearValue += g.PriceCents
		}
	}

	var siteCfg models.SiteConfig
	db.DB.Limit(1).Find(&siteCfg)
	initialLiquid := siteCfg.InitialLiquidBalance

	// 使用 SQL 聚合计算收入与非分期支出总额，杜绝全表 Transaction 加载进内存
	var totalIncome int64
	db.DB.Model(&models.Transaction{}).
		Where("type = 'income'").
		Select("COALESCE(SUM(amount), 0)").
		Scan(&totalIncome)

	var nonInstallmentExpense int64
	db.DB.Model(&models.Transaction{}).
		Where("type = 'expense' AND (is_installment = ? OR is_installment IS NULL)", false).
		Select("COALESCE(SUM(amount), 0)").
		Scan(&nonInstallmentExpense)

	// 仅查询分期账目进行分期还款核算与信贷渠道聚合
	var installmentTxs []models.Transaction
	db.DB.Where("type = 'expense' AND is_installment = ?", true).Find(&installmentTxs)

	var totalLiabilities int64
	var monthlyLiabilities int64

	// 加载所有信贷渠道配置
	var channels []models.CreditChannel
	db.DB.Order("id ASC").Find(&channels)

	channelMap := make(map[string]*ChannelLiabilitySummary)
	var channelOrder []string

	for _, ch := range channels {
		summary := &ChannelLiabilitySummary{
			ChannelCode:        ch.Code,
			ChannelName:        ch.Name,
			RepaymentDay:       ch.RepaymentDay,
			Color:              ch.Color,
			TotalDebt:          0,
			MonthlyPay:         0,
			DaysUntilRepayment: calculateDaysUntilRepayment(ch.RepaymentDay),
			ActiveCount:        0,
			Installments:       make([]models.Transaction, 0),
		}
		channelMap[ch.Code] = summary
		channelOrder = append(channelOrder, ch.Code)
	}

	now := time.Now()
	for i := range installmentTxs {
		tx := &installmentTxs[i]
		chCode := tx.InstallmentChannelCode
		if chCode == "" {
			chCode = "huabei"
		}

		repaymentDay := tx.RepaymentDay
		if summary, exists := channelMap[chCode]; exists && summary.RepaymentDay > 0 {
			repaymentDay = summary.RepaymentDay
		}
		if repaymentDay <= 0 {
			repaymentDay = 10
		}

		// 根据购买时间 (tx.OccurredAt) 与渠道每月还款日自动核算已还期数与剩余期数
		_, autoRemain := CalculateElapsedInstallmentMonths(tx.OccurredAt, tx.InstallmentMonths, repaymentDay, now)
		needSave := false
		if tx.InstallmentRemainMonths > autoRemain {
			tx.InstallmentRemainMonths = autoRemain
			needSave = true
		}
		if tx.RepaymentDay != repaymentDay {
			tx.RepaymentDay = repaymentDay
			needSave = true
		}
		if needSave {
			_ = db.DB.Save(tx).Error
		}

		paidMonths := tx.InstallmentMonths - tx.InstallmentRemainMonths
		if paidMonths < 0 {
			paidMonths = 0
		}
		totalAmount := tx.InstallmentTotalAmount
		if totalAmount <= 0 {
			totalAmount = tx.Amount
		}
		_, debt := CalculateInstallmentAmounts(totalAmount, tx.InstallmentMonths, tx.InstallmentMonthlyPay, paidMonths, tx.InstallmentRemainMonths)

		// 剩余期数 > 0 计入待还负债
		if tx.InstallmentRemainMonths > 0 {
			totalLiabilities += debt
			monthlyLiabilities += tx.InstallmentMonthlyPay

			summary, exists := channelMap[chCode]
			if !exists {
				summary = &ChannelLiabilitySummary{
					ChannelCode:        chCode,
					ChannelName:        chCode,
					RepaymentDay:       repaymentDay,
					Color:              "var(--accent-warm)",
					TotalDebt:          0,
					MonthlyPay:         0,
					DaysUntilRepayment: calculateDaysUntilRepayment(repaymentDay),
					ActiveCount:        0,
					Installments:       make([]models.Transaction, 0),
				}
				channelMap[chCode] = summary
				channelOrder = append(channelOrder, chCode)
			}
			summary.TotalDebt += debt
			summary.MonthlyPay += tx.InstallmentMonthlyPay
			summary.ActiveCount++
			summary.Installments = append(summary.Installments, *tx)
		}
	}

	liquidBalance := initialLiquid + totalIncome - nonInstallmentExpense

	var channelLiabilities []ChannelLiabilitySummary
	for _, code := range channelOrder {
		if s, ok := channelMap[code]; ok {
			channelLiabilities = append(channelLiabilities, *s)
		}
	}

	fixedAssets := activeGearValue
	netWorth := liquidBalance + fixedAssets - totalLiabilities

	res := AssetsOverviewResponse{
		NetWorth:             netWorth,
		LiquidBalance:        liquidBalance,
		InitialLiquidBalance: initialLiquid,
		FixedAssets:          fixedAssets,
		TotalLiabilities:     totalLiabilities,
		MonthlyLiabilities:   monthlyLiabilities,
		GearSummary: GearSummary{
			TotalCount:         len(gearList),
			ActiveCount:        activeGearCount,
			ActiveValue:        activeGearValue,
			SoldCount:          soldGearCount,
			SoldRecoveredValue: soldRecoveredValue,
			Items:              gearList,
		},
		ChannelLiabilities: channelLiabilities,
	}

	Success(c, res)
}

// SetLiquidBalanceRequest 设定初始流动资产底数入参 (BE-15)
type SetLiquidBalanceRequest struct {
	InitialLiquidBalance int64 `json:"initial_liquid_balance"`
}

// SetInitialLiquidBalance 设定初始流动资产底数 (BE-15)
// PUT /api/v1/assets/liquid-balance
func SetInitialLiquidBalance(c *gin.Context) {
	var req SetLiquidBalanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	var cfg models.SiteConfig
	if err := db.DB.FirstOrCreate(&cfg).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取配置失败: "+err.Error())
		return
	}

	cfg.InitialLiquidBalance = req.InitialLiquidBalance
	cfg.UpdatedAt = time.Now()
	if err := db.DB.Save(&cfg).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新初始流动资产底数失败: "+err.Error())
		return
	}

	Success(c, gin.H{
		"initial_liquid_balance": cfg.InitialLiquidBalance,
	})
}

// AddGearAssetRequest 录入新设备单品请求 (RC-53)
type AddGearAssetRequest struct {
	Name                   string `json:"name" binding:"required"`
	Category               string `json:"category" binding:"required"`
	Spec                   string `json:"spec"`
	PriceCents             int64  `json:"price_cents"`
	PurchaseDate           string `json:"purchase_date"`
	PaymentMethod          string `json:"payment_method"`
	Note                   string `json:"note"`
	CreateTransaction      bool   `json:"create_transaction"` // 默认支持同步生成支出流水
	IsInstallment          bool   `json:"is_installment"`
	InstallmentChannelCode string `json:"installment_channel_code"`
	InstallmentTotalAmount int64  `json:"installment_total_amount"`
	InstallmentMonths      int    `json:"installment_months"`
	RepaymentDay           int    `json:"repayment_day"`
}

// AddGearAsset 录入随身/桌面硬件设备至固资库 (买入即记账双向联动 RC-53)
func AddGearAsset(c *gin.Context) {
	var req AddGearAssetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "设备名称与分类必填")
		return
	}

	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		Error(c, http.StatusNotFound, "主理人档案尚未初始化")
		return
	}

	purchaseDate := strings.TrimSpace(req.PurchaseDate)
	if purchaseDate == "" {
		purchaseDate = time.Now().Format("2006-01-02")
	}

	gearID := fmt.Sprintf("gear_%s", generateRandomHex(8))
	newGear := models.GearItem{
		ID:           gearID,
		Name:         strings.TrimSpace(req.Name),
		Spec:         strings.TrimSpace(req.Spec),
		Category:     strings.TrimSpace(req.Category),
		Status:       "active",
		PriceCents:   req.PriceCents,
		PurchaseDate: purchaseDate,
		Note:         strings.TrimSpace(req.Note),
	}

	list := loadProfileGear(&profile)
	list = append(list, newGear)
	if err := saveProfileGear(&profile, list); err != nil {
		Error(c, http.StatusInternalServerError, "保存设备资产失败: "+err.Error())
		return
	}

	// 若勾选了同步生成支出账单或启用了分期购置，且金额 > 0，自动生成流水
	if (req.CreateTransaction || req.IsInstallment) && req.PriceCents > 0 {
		occurred, err := ParseFlexTime(purchaseDate)
		if err != nil {
			occurred = time.Now()
		}

		tx := models.Transaction{
			Amount:        req.PriceCents,
			Type:          "expense",
			Category:      req.Category,
			Title:         "购置设备: " + newGear.Name,
			Note:          fmt.Sprintf("录入设备固资库自动同步流水 (%s)", newGear.Spec),
			PaymentMethod: strings.TrimSpace(req.PaymentMethod),
			OccurredAt:    occurred,
		}
		if tx.PaymentMethod == "" {
			tx.PaymentMethod = "设备购置"
		}

		if req.IsInstallment {
			AssembleInstallmentTransaction(
				&tx,
				req.IsInstallment,
				req.InstallmentChannelCode,
				req.InstallmentTotalAmount,
				req.PriceCents,
				req.InstallmentMonths,
				0,
				0,
				req.RepaymentDay,
				db.DB,
			)
		}

		if err := db.DB.Create(&tx).Error; err != nil {
			Error(c, http.StatusInternalServerError, "设备已录入但同步生成购置账单失败: "+err.Error())
			return
		}
	}

	Success(c, newGear)
}

// SellGearAssetRequest 设备转售出二手请求 (RC-53, RC-79)
type SellGearAssetRequest struct {
	SoldPriceCents int64  `json:"sold_price_cents"` // 出手回血金额 (分)
	SoldDate       string `json:"sold_date"`        // 转让日期 (YYYY-MM-DD)
	Note           string `json:"note"`             // 出手备注
}

// SellGearAsset 设备出二手/转让 (自动从在用固资扣减并生成回血收入账单 RC-53)
func SellGearAsset(c *gin.Context) {
	gearID := c.Param("id")
	var req SellGearAssetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "卖出回血金额必填")
		return
	}
	soldCents := req.SoldPriceCents
	if soldCents <= 0 {
		Error(c, http.StatusBadRequest, "卖出回血金额必填")
		return
	}

	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		Error(c, http.StatusNotFound, "主理人档案尚未初始化")
		return
	}

	list := loadProfileGear(&profile)
	foundIdx := -1
	for i, g := range list {
		if g.ID == gearID {
			foundIdx = i
			break
		}
	}

	if foundIdx == -1 {
		Error(c, http.StatusNotFound, "设备不存在")
		return
	}

	soldDate := strings.TrimSpace(req.SoldDate)
	if soldDate == "" {
		soldDate = time.Now().Format("2006-01-02")
	}

	list[foundIdx].Status = "sold"
	list[foundIdx].SoldPriceCents = soldCents
	list[foundIdx].SoldDate = soldDate
	if req.Note != "" {
		if list[foundIdx].Note != "" {
			list[foundIdx].Note += " | " + strings.TrimSpace(req.Note)
		} else {
			list[foundIdx].Note = strings.TrimSpace(req.Note)
		}
	}

	if err := saveProfileGear(&profile, list); err != nil {
		Error(c, http.StatusInternalServerError, "更新设备状态失败: "+err.Error())
		return
	}

	// 自动生成一条「二手回血」收入流水，实时补充可用余额
	occurred, err := ParseFlexTime(soldDate)
	if err != nil {
		occurred = time.Now()
	}

	incomeTx := models.Transaction{
		Amount:        soldCents,
		Type:          "income",
		Category:      "二手回血",
		Title:         "设备转售回血: " + list[foundIdx].Name,
		Note:          fmt.Sprintf("设备出二手流转回血 (%s)", strings.TrimSpace(req.Note)),
		PaymentMethod: "二手转让",
		OccurredAt:    occurred,
	}
	if err := db.DB.Create(&incomeTx).Error; err != nil {
		Error(c, http.StatusInternalServerError, "设备已转售但生成二手回血账单失败: "+err.Error())
		return
	}

	Success(c, list[foundIdx])
}

// DeleteGearAsset 从设备库中物理删除设备单品 (RC-53)
func DeleteGearAsset(c *gin.Context) {
	gearID := c.Param("id")

	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		Error(c, http.StatusNotFound, "主理人档案尚未初始化")
		return
	}

	list := loadProfileGear(&profile)
	foundIdx := -1
	for i, g := range list {
		if g.ID == gearID {
			foundIdx = i
			break
		}
	}

	if foundIdx == -1 {
		Error(c, http.StatusNotFound, "设备不存在")
		return
	}

	// 移除该项
	list = append(list[:foundIdx], list[foundIdx+1:]...)
	if err := saveProfileGear(&profile, list); err != nil {
		Error(c, http.StatusInternalServerError, "删除设备失败: "+err.Error())
		return
	}

	Success(c, gin.H{"id": gearID, "deleted": true})
}
