package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// errGearAlreadySold 设备已转售（发现 12）：重复转售会在资产库状态不变的情况下
// 再生成一条等额「二手回血」收入流水，收入与净资产虚增 —— 直接拒绝。
var errGearAlreadySold = errors.New("该设备已转售，不能重复转售")

// ChannelLiabilitySummary 信贷渠道负债聚合摘要 (RC-53)
type ChannelLiabilitySummary struct {
	ChannelCode        string               `json:"channel_code"`
	ChannelName        string               `json:"channel_name"`
	RepaymentDay       int                  `json:"repayment_day"`
	Color              string               `json:"color"`
	TotalDebt          int64                `json:"total_debt"`           // 该渠道剩余待还总额（分）
	MonthlyPay         int64                `json:"monthly_pay"`          // 每月待还总额（分）
	DaysUntilRepayment int                  `json:"days_until_repayment"` // 距离下次还款日天数倒计时
	ActiveCount        int                  `json:"active_count"`         // 活跃分期笔数
	Installments       []models.Transaction `json:"installments"`         // 关联分期账目流水列表
}

// GearSummary 随身与固定资产设备库摘要 (RC-53)
type GearSummary struct {
	TotalCount         int               `json:"total_count"`
	ActiveCount        int               `json:"active_count"`
	ActiveValue        int64             `json:"active_value"` // 在用固资总价值（分）
	SoldCount          int               `json:"sold_count"`
	SoldRecoveredValue int64             `json:"sold_recovered_value"` // 二手转让回血总额（分）
	Items              []models.GearItem `json:"items"`
}

// AssetsOverviewResponse 资产大盘看板聚合响应 (RC-53, BE-15)
type AssetsOverviewResponse struct {
	NetWorth             int64                     `json:"net_worth"`              // 个人净资产 = 流动可用余额 + 固定资产沉淀 - 待还信贷负债
	LiquidBalance        int64                     `json:"liquid_balance"`         // 流动可用现金余额
	InitialLiquidBalance int64                     `json:"initial_liquid_balance"` // 初始流动现金底数 (BE-15)
	FixedAssets          int64                     `json:"fixed_assets"`           // 在用固定设备资产沉淀
	TotalLiabilities     int64                     `json:"total_liabilities"`      // 待还信贷分期总负债
	MonthlyLiabilities   int64                     `json:"monthly_liabilities"`    // 每月固定还款支出
	GearSummary          GearSummary               `json:"gear_summary"`           // 设备库全生命周期盘点
	ChannelLiabilities   []ChannelLiabilitySummary `json:"channel_liabilities"`    // 信贷渠道明细与独立还款日倒计时
}

// calculateDaysUntilRepayment 计算距离每月独立还款日的天数倒计时（按北京时间计算 RC-134）
func calculateDaysUntilRepayment(repaymentDay int) int {
	if repaymentDay < 1 {
		repaymentDay = 1
	}
	if repaymentDay > 31 {
		repaymentDay = 31
	}
	now := config.NowInBeijing()
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

// profileGearMu 串行化 CuratorProfile.EDCGear 同一行 JSON 列的「读-改-写」，
// 杜绝并发录入设备 / 记账联动建资产 / 心愿归册建资产互相覆盖丢条目 (BE-34)
var profileGearMu sync.Mutex

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

func saveProfileGearWith(dbTx *gorm.DB, profile *models.CuratorProfile, list []models.GearItem) error {
	bytes, err := json.Marshal(list)
	if err != nil {
		return err
	}
	profile.EDCGear = string(bytes)
	profile.UpdatedAt = config.NowInBeijing()
	return dbTx.Save(profile).Error
}

func saveProfileGear(profile *models.CuratorProfile, list []models.GearItem) error {
	return saveProfileGearWith(db.DB, profile, list)
}

// mutateProfileGearLocked 在**已持有 profileGearMu** 时用指定连接读写 EDCGear。
//
// 事务调用方必须先取锁再开启事务：锁序固定为 profileGearMu → DB 事务，
// 否则「事务持写锁等互斥锁、持锁方等写锁」会在 SQLite 单写者模型下互相卡住。
func mutateProfileGearLocked(dbTx *gorm.DB, mutate func([]models.GearItem) ([]models.GearItem, error)) error {
	var profile models.CuratorProfile
	if err := dbTx.First(&profile).Error; err != nil {
		return err
	}
	list, err := mutate(loadProfileGear(&profile))
	if err != nil {
		return err
	}
	return saveProfileGearWith(dbTx, &profile, list)
}

// mutateProfileGear 在进程级互斥锁内完成 EDCGear 的读-改-写（事务外场景的统一入口）
func mutateProfileGear(mutate func([]models.GearItem) ([]models.GearItem, error)) error {
	profileGearMu.Lock()
	defer profileGearMu.Unlock()
	return mutateProfileGearLocked(db.DB, mutate)
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
		if g.Status == "sold" {
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
	// 已还分期款合计（逐期扣现金时要从流动余额里减掉）
	var totalRepaid int64

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

	// 分期剩余期数按当前时间即时推算展示，不落库 (BE-24：GET 保持只读语义)
	now := config.NowInBeijing()
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

		// 根据购买时间 (tx.OccurredAt) 与渠道每月还款日自动核算已还期数与剩余期数（仅内存校正，不落库）
		_, autoRemain := CalculateElapsedInstallmentMonths(tx.OccurredAt, tx.InstallmentMonths, repaymentDay, now)
		if tx.InstallmentRemainMonths > autoRemain {
			tx.InstallmentRemainMonths = autoRemain
		}
		tx.RepaymentDay = repaymentDay

		paidMonths := tx.InstallmentMonths - tx.InstallmentRemainMonths
		if paidMonths < 0 {
			paidMonths = 0
		}
		totalAmount := tx.InstallmentTotalAmount
		if totalAmount <= 0 {
			totalAmount = tx.Amount
		}
		paidAmount, debt := CalculateInstallmentAmounts(totalAmount, tx.InstallmentMonths, tx.InstallmentMonthlyPay, paidMonths, tx.InstallmentRemainMonths)
		// 关键（DEC-01 口径 B：分期款**逐期**扣现金）：
		// 每还一期，现金减一次、负债也减一次 ⇒ 净资产始终不变。
		// 原先分期支出**完全不算**现金支出，而负债却逐月减少，
		// 两套模型混用 ⇒ 净资产随还款进度**虚增**（涨的幅度恰好等于已还金额）。
		totalRepaid += paidAmount

		// 剩余期数 > 0 计入待还负债
		if tx.InstallmentRemainMonths > 0 {
			totalLiabilities += debt
			// 关键（DEC-02 口径 A）：「本月应还」按**下一期的实际金额**算 ——
			// 最后一期会多出整除的尾差。
			// 早先直接累加 monthlyPay（整除值），
			// 于是「月供 × 期数 ≠ 总额」，界面上两个数字对不上。
			nextPeriod := paidMonths + 1
			monthlyLiabilities += InstallmentPeriodAmount(
				totalAmount, tx.InstallmentMonths, tx.InstallmentMonthlyPay, nextPeriod,
			)

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
			// 与「本月应还」同口径：按下一期的实际金额累加，末期限吃掉整除尾差。
			summary.MonthlyPay += InstallmentPeriodAmount(
				totalAmount, tx.InstallmentMonths, tx.InstallmentMonthlyPay, paidMonths+1,
			)
			summary.ActiveCount++
			summary.Installments = append(summary.Installments, *tx)
		}
	}

	// 关键（DEC-01 口径 B）：流动可用余额要扣掉**已还的分期款**。
	// 购入时现金不动（分期还没还），每还一期现金减一次、负债减一次，
	// 于是净资产在整个分期周期内**恒定**。
	liquidBalance := initialLiquid + totalIncome - nonInstallmentExpense - totalRepaid

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
	Name string `json:"name" binding:"required"`
	// MOB-P1-35：省略即默认「数码器材」
	Category               string `json:"category"`
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

	// 设备价格不能为负（兜底校验修复）：负价格会污染在用固资价值（联动流水本身有 >0 保护）
	if req.PriceCents < 0 {
		Error(c, http.StatusBadRequest, "设备价格不能为负数")
		return
	}
	// 分期参数兜底校验（与记账入口同口径）：期数上限、渠道真实存在。
	if req.IsInstallment {
		if req.InstallmentMonths > installmentMonthsLimit {
			Error(c, http.StatusBadRequest, "分期期数上限为 60 期")
			return
		}
		if !InstallmentChannelExists(nil, req.InstallmentChannelCode) {
			Error(c, http.StatusBadRequest, "分期渠道不存在")
			return
		}
	}

	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		Error(c, http.StatusNotFound, "主理人档案尚未初始化")
		return
	}

	purchaseDate := strings.TrimSpace(req.PurchaseDate)
	if purchaseDate == "" {
		purchaseDate = config.TodayInBeijing()
	}

	gearID := fmt.Sprintf("gear_%s", generateRandomHex(8))
	// MOB-P1-35：省略分类时回落到默认「数码器材」
	gearCategory := strings.TrimSpace(req.Category)
	if gearCategory == "" {
		gearCategory = "数码器材"
	}

	newGear := models.GearItem{
		ID:           gearID,
		Name:         strings.TrimSpace(req.Name),
		Spec:         strings.TrimSpace(req.Spec),
		Category:     gearCategory,
		Status:       "active",
		PriceCents:   req.PriceCents,
		PurchaseDate: purchaseDate,
		Note:         strings.TrimSpace(req.Note),
	}

	// 关键：三段写入（档案 EDCGear / 伴生散页 / 联动账单）必须同生共死 ——
	// 任一失败整体回滚，消除「设备已录入但伴生散页或账单失败」的部分成功分支
	//（中途失败后重试会产生重复设备）。
	// 先取器物互斥锁，再开启数据库事务（锁序 profileGearMu → DB，
	// 与 CreateTransaction / SellGearAsset 对齐）。
	profileGearMu.Lock()
	defer profileGearMu.Unlock()

	if err := db.DB.Transaction(func(dbTx *gorm.DB) error {
		if err := mutateProfileGearLocked(dbTx, func(list []models.GearItem) ([]models.GearItem, error) {
			return append(list, newGear), nil
		}); err != nil {
			return err
		}

		// 伴生创建机制：添置器物时在时间流落一条对应的一级散页，
		// 使「所有二级对象都能在时间流里查到」这一约定成立。
		gearOccurredAt, parseErr := ParseFlexTime(purchaseDate)
		if parseErr != nil {
			gearOccurredAt = config.NowInBeijing()
		}
		if _, err := createGearCompanionEntry(dbTx, newGear, gearOccurredAt); err != nil {
			return err
		}

		// 若勾选了同步生成支出账单或启用了分期购置，且金额 > 0，自动生成流水
		if (req.CreateTransaction || req.IsInstallment) && req.PriceCents > 0 {
			occurred, parseErr := ParseFlexTime(purchaseDate)
			if parseErr != nil {
				occurred = config.NowInBeijing()
			}

			linkedTx := models.Transaction{
				Amount: req.PriceCents,
				Type:   "expense",
				// 发现 21：联动账单用**兜底后**的分类（省略 category 时应为「数码器材」），
				// 原先用原始 req.Category，省略时会落一条空分类账目，分类统计出现无名项。
				Category:      gearCategory,
				Title:         "购置设备: " + newGear.Name,
				Note:          fmt.Sprintf("录入设备固资库自动同步流水 (%s)", newGear.Spec),
				PaymentMethod: strings.TrimSpace(req.PaymentMethod),
				OccurredAt:    occurred,
			}
			if linkedTx.PaymentMethod == "" {
				linkedTx.PaymentMethod = "设备购置"
			}

			if req.IsInstallment {
				AssembleInstallmentTransaction(
					&linkedTx,
					req.IsInstallment,
					req.InstallmentChannelCode,
					req.InstallmentTotalAmount,
					req.PriceCents,
					req.InstallmentMonths,
					0,
					0,
					req.RepaymentDay,
					dbTx,
				)
			}

			if err := dbTx.Create(&linkedTx).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		Error(c, http.StatusInternalServerError, "录入设备失败，已整体回滚: "+err.Error())
		return
	}

	// 伴生散页（total_entries）与联动账目（month_expense）都改变了 /stats 口径：
	// 统计缓存必须失效，与其他写路径同口径（第三轮 P2-6）。
	InvalidateStatsCache()

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

	soldDate := strings.TrimSpace(req.SoldDate)
	if soldDate == "" {
		soldDate = config.TodayInBeijing()
	}

	var soldGear models.GearItem
	found := false
	occurred, err := ParseFlexTime(soldDate)
	if err != nil {
		occurred = time.Now()
	}

	// 设备状态、回血流水与伴生散页必须原子提交：任一失败都回滚，
	// 否则会出现「已标记售出却没有收入」且重试被已售守卫永久拒绝。
	// 先取器物互斥锁，再开启事务（锁序 profileGearMu → DB）。
	profileGearMu.Lock()
	defer profileGearMu.Unlock()

	err = db.DB.Transaction(func(tx *gorm.DB) error {
		if err := mutateProfileGearLocked(tx, func(list []models.GearItem) ([]models.GearItem, error) {
			for i := range list {
				if list[i].ID == gearID {
					if list[i].Status == "sold" {
						return nil, errGearAlreadySold
					}
					list[i].Status = "sold"
					list[i].SoldPriceCents = soldCents
					list[i].SoldDate = soldDate
					if req.Note != "" {
						if list[i].Note != "" {
							list[i].Note += " | " + strings.TrimSpace(req.Note)
						} else {
							list[i].Note = strings.TrimSpace(req.Note)
						}
					}
					soldGear = list[i]
					found = true
					return list, nil
				}
			}
			return nil, fmt.Errorf("设备不存在")
		}); err != nil {
			return err
		}

		incomeTx := models.Transaction{
			Amount:        soldCents,
			Type:          "income",
			Category:      "二手回血",
			Title:         "设备转售回血: " + soldGear.Name,
			Note:          fmt.Sprintf("设备出二手流转回血 (%s)", strings.TrimSpace(req.Note)),
			PaymentMethod: "二手转让",
			OccurredAt:    occurred,
		}
		if err := tx.Create(&incomeTx).Error; err != nil {
			return err
		}

		// 同步器物伴生散页：转手后原记录要反映「已转手」，
		// 否则时间流里的那条「添置器物 · xxx」看起来器物还在手上。
		return tx.Model(&models.LifeEntry{}).
			Where("gear_id = ?", soldGear.ID).
			Updates(map[string]any{
				"content":    buildGearCompanionContent(soldGear) + fmt.Sprintf(" 已于 %s 转手，回血 ¥%.2f。", soldDate, float64(soldCents)/100),
				"updated_at": time.Now(),
			}).Error
	})
	if err != nil {
		if errors.Is(err, errGearAlreadySold) {
			// 跨端契约：重复转售按 400 拒绝，防双击/重试重复回血（发现 12）
			Error(c, http.StatusBadRequest, errGearAlreadySold.Error())
			return
		}
		if !found {
			Error(c, http.StatusNotFound, "设备不存在")
		} else {
			Error(c, http.StatusInternalServerError, "转售失败，已回滚: "+err.Error())
		}
		return
	}

	InvalidateStatsCache()
	Success(c, soldGear)
}

// RestoreGearAssetRequest 恢复一件被删除的随身设备（⌘Z 撤销用）
//
// 关键：设备本体存在**档案的 JSON 列**里（`CuratorProfile.edcGear`），
// 删除是「从数组里移除」—— 没有 deleted_at 可标记，也就**没有恢复的落点**。
// 所以恢复时由客户端把**原始设备数据**带回来（撤销栈里还留着它），
// 服务端按**原 id** 重新插回数组，并连带恢复它的伴生散页。
type RestoreGearAssetRequest struct {
	ID             string `json:"id" binding:"required"`
	Name           string `json:"name" binding:"required"`
	Spec           string `json:"spec"`
	Category       string `json:"category"`
	Status         string `json:"status"`
	PriceCents     int64  `json:"price_cents"`
	PurchaseDate   string `json:"purchase_date"`
	SoldPriceCents int64  `json:"sold_price_cents"`
	SoldDate       string `json:"sold_date"`
	Note           string `json:"note"`
	// Index 原先在档案数组中的位置（-1 = 追加到末尾）
	Index int `json:"index"`
}

// RestoreGearAsset 恢复一件被删除的随身设备（连同它的伴生散页）
//
// 关键：这是「删除 ⌘Z 可撤销」的后端前提。
// 原先删除是**硬删**（从 JSON 数组里移除），伴生散页也一并软删，
// 而**软删除的散页没有任何恢复端点** —— 于是误删一件设备，
// 它的照片与手记就**永久回不来了**。
//
// 现在恢复做两件事：
// 1. 把设备按**原 id** 插回档案数组（原位置优先）；
// 2. 把 `gear_id` 指向它的伴生散页**取消软删除**（`deleted_at` 置空）。
func RestoreGearAsset(c *gin.Context) {
	var req RestoreGearAssetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "设备 id 与名称必填")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		Error(c, http.StatusBadRequest, "设备 id 必填")
		return
	}

	restored := false
	err := mutateProfileGear(func(list []models.GearItem) ([]models.GearItem, error) {
		// 已经存在（可能重复提交）⇒ 幂等返回，不重复插入
		for _, g := range list {
			if g.ID == req.ID {
				restored = true
				return list, nil
			}
		}
		item := models.GearItem{
			ID:             req.ID,
			Name:           strings.TrimSpace(req.Name),
			Spec:           req.Spec,
			Category:       req.Category,
			Status:         req.Status,
			PriceCents:     req.PriceCents,
			PurchaseDate:   req.PurchaseDate,
			SoldPriceCents: req.SoldPriceCents,
			SoldDate:       req.SoldDate,
			Note:           req.Note,
		}
		idx := req.Index
		if idx < 0 || idx > len(list) {
			idx = len(list)
		}
		restored = true
		return append(list[:idx], append([]models.GearItem{item}, list[idx:]...)...), nil
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "恢复设备失败: "+err.Error())
		return
	}
	if !restored {
		Error(c, http.StatusNotFound, "设备不存在")
		return
	}

	// 连带恢复伴生散页（软删除的清掉 deleted_at）
	if err := db.DB.Unscoped().Model(&models.LifeEntry{}).
		Where("gear_id = ?", req.ID).
		Update("deleted_at", nil).Error; err != nil {
		Error(c, http.StatusInternalServerError, "设备已恢复但伴生记录恢复失败: "+err.Error())
		return
	}

	// 伴生散页取消软删改变了 total_entries：/stats 缓存必须失效（第三轮 P2-6）
	InvalidateStatsCache()

	Success(c, gin.H{"id": req.ID, "restored": true})
}

// DeleteGearAsset 从设备库中物理删除设备单品 (RC-53)
func DeleteGearAsset(c *gin.Context) {
	gearID := c.Param("id")

	found := false
	if err := mutateProfileGear(func(list []models.GearItem) ([]models.GearItem, error) {
		for i := range list {
			if list[i].ID == gearID {
				found = true
				return append(list[:i], list[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("设备不存在")
	}); err != nil {
		if !found {
			Error(c, http.StatusNotFound, "设备不存在")
		} else {
			Error(c, http.StatusInternalServerError, "删除设备失败: "+err.Error())
		}
		return
	}

	// 关键：删除器物时一并软删除它在时间流里的伴生散页，避免留下孤儿记录。
	if err := db.DB.Transaction(func(tx *gorm.DB) error {
		return deleteGearCompanionEntries(tx, gearID)
	}); err != nil {
		Error(c, http.StatusInternalServerError, "设备已删除但清理伴生记录失败: "+err.Error())
		return
	}

	// 伴生散页被软删除后 total_entries 随之变化，统计缓存必须失效。
	InvalidateStatsCache()

	Success(c, gin.H{"id": gearID, "deleted": true})
}
