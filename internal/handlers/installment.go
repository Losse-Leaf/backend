package handlers

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SplitInstallmentPeriods 均摊计算：本金与手续费都按整数分平均，余数加到最后一期 (RC-140)
//
// 手续费模式：
// - "none": 无手续费，每期 0
// - "fixed_per_period": 每期固定金额 (feeValue 为每期金额，单位分)
// - "rate_per_period": 每期费率 (feeValue 为基点 bps，例如 50 代表 0.50%，按初始总本金计算)
// - "total_fee": 总手续费均摊 (feeValue 为总手续费，单位分，按整数分平均，余数加到最后一期)
func SplitInstallmentPeriods(principal int64, periods int, feeMode string, feeValue int64) (principalParts []int64, feeParts []int64, totalFee int64, err error) {
	if principal <= 0 {
		return nil, nil, 0, errors.New("本金必须大于 0")
	}
	if periods <= 0 || periods > 120 {
		return nil, nil, 0, errors.New("期数必须在 1 到 120 之间")
	}

	principalParts = make([]int64, periods)
	feeParts = make([]int64, periods)

	// 本金均摊：前 N-1 期整除，余数加到最后一期
	baseP := principal / int64(periods)
	remP := principal % int64(periods)
	for i := 0; i < periods; i++ {
		if i == periods-1 {
			principalParts[i] = baseP + remP
		} else {
			principalParts[i] = baseP
		}
	}

	// 手续费均摊
	switch feeMode {
	case "none", "":
		totalFee = 0
		for i := 0; i < periods; i++ {
			feeParts[i] = 0
		}
	case "fixed_per_period":
		if feeValue < 0 {
			return nil, nil, 0, errors.New("手续费金额不能为负数")
		}
		totalFee = feeValue * int64(periods)
		for i := 0; i < periods; i++ {
			feeParts[i] = feeValue
		}
	case "rate_per_period":
		if feeValue < 0 {
			return nil, nil, 0, errors.New("手续费费率不能为负数")
		}
		// feeValue 为基点 (bps, 1% = 100 bps)
		// 单期手续费 = round(principal * feeValue / 10000)
		feePerPeriod := (principal*feeValue + 5000) / 10000
		totalFee = feePerPeriod * int64(periods)
		for i := 0; i < periods; i++ {
			feeParts[i] = feePerPeriod
		}
	case "total_fee":
		if feeValue < 0 {
			return nil, nil, 0, errors.New("总手续费不能为负数")
		}
		totalFee = feeValue
		baseF := feeValue / int64(periods)
		remF := feeValue % int64(periods)
		for i := 0; i < periods; i++ {
			if i == periods-1 {
				feeParts[i] = baseF + remF
			} else {
				feeParts[i] = baseF
			}
		}
	default:
		return nil, nil, 0, fmt.Errorf("不支持的手续费模式: %s", feeMode)
	}

	return principalParts, feeParts, totalFee, nil
}

// daysInMonth 返回指定年月的总天数 (根据北京时间)
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, config.BeijingLocation).Day()
}

// CalculateDueDate 计算指定期次的还款日（第 1 期取 firstDueDate，后续各期根据 repayDay 自动顺延并处理月末截断）
func CalculateDueDate(firstDueDate string, repayDay int, seq int) (string, error) {
	if seq <= 0 {
		return "", errors.New("期次必须从 1 开始")
	}
	if repayDay < 1 {
		repayDay = 1
	}
	if repayDay > 31 {
		repayDay = 31
	}

	loc := config.BeijingLocation
	firstT, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(firstDueDate), loc)
	if err != nil {
		return "", fmt.Errorf("首期还款日格式无效 (需 YYYY-MM-DD): %v", err)
	}

	if seq == 1 {
		return firstT.Format("2006-01-02"), nil
	}

	// 从首期日期出发，顺延 seq - 1 个月
	baseYear, baseMonth, _ := firstT.Date()
	targetMonthTotal := int(baseMonth) + (seq - 1)
	targetYear := baseYear + (targetMonthTotal-1)/12
	targetMonthVal := (targetMonthTotal-1)%12 + 1
	targetMonth := time.Month(targetMonthVal)

	maxDay := daysInMonth(targetYear, targetMonth)
	day := repayDay
	if day > maxDay {
		day = maxDay // 还款日大于当月天数时取当月最后一天 (口径 6)
	}

	targetDate := time.Date(targetYear, targetMonth, day, 0, 0, 0, 0, loc)
	return targetDate.Format("2006-01-02"), nil
}

// CreatePlanForTransactionTx 在事务中为记账流水建立真实分期计划与各期明细
func CreatePlanForTransactionTx(
	tx *gorm.DB,
	tr *models.Transaction,
	periods int,
	channelCode string,
	feeMode string,
	feeValue int64,
	firstDueDate string,
	repayDay int,
	kind string,
	assetID string,
) (*models.InstallmentPlan, []models.InstallmentItem, error) {
	if !tx.Migrator().HasTable(&models.InstallmentPlan{}) || !tx.Migrator().HasTable(&models.InstallmentItem{}) {
		return nil, nil, nil
	}
	if periods <= 0 {
		periods = 12
	}
	if periods > 120 {
		periods = 120
	}
	if kind == "" {
		if assetID != "" {
			kind = "asset"
		} else {
			kind = "expense"
		}
	}
	if feeMode == "" {
		feeMode = "none"
	}
	if feeValue < 0 {
		feeValue = 0
	}

	// 查找信贷渠道
	var channel models.CreditChannel
	channelCode = strings.TrimSpace(channelCode)
	if channelCode != "" {
		if err := tx.First(&channel, "code = ?", channelCode).Error; err != nil {
			_ = tx.Order("id ASC").First(&channel).Error
		}
	} else {
		_ = tx.Order("id ASC").First(&channel).Error
	}

	if repayDay < 1 || repayDay > 31 {
		repayDay = channel.RepaymentDay
	}
	if repayDay < 1 {
		repayDay = 10
	}

	now := config.NowInBeijing()
	firstDueDate = strings.TrimSpace(firstDueDate)
	if firstDueDate == "" {
		txDate := tr.OccurredAt.In(config.BeijingLocation)
		if txDate.IsZero() {
			txDate = now
		}
		y, m, d := txDate.Date()
		if d < repayDay {
			maxD := daysInMonth(y, m)
			actualD := repayDay
			if actualD > maxD {
				actualD = maxD
			}
			firstDueDate = time.Date(y, m, actualD, 0, 0, 0, 0, config.BeijingLocation).Format("2006-01-02")
		} else {
			nextMonth := txDate.AddDate(0, 1, 0)
			ny, nm, _ := nextMonth.Date()
			maxD := daysInMonth(ny, nm)
			actualD := repayDay
			if actualD > maxD {
				actualD = maxD
			}
			firstDueDate = time.Date(ny, nm, actualD, 0, 0, 0, 0, config.BeijingLocation).Format("2006-01-02")
		}
	}

	principalParts, feeParts, totalFee, err := SplitInstallmentPeriods(tr.Amount, periods, feeMode, feeValue)
	if err != nil {
		return nil, nil, err
	}

	title := strings.TrimSpace(tr.Title)
	if title == "" {
		title = tr.Category
	}

	plan := models.InstallmentPlan{
		Title:           title,
		ChannelID:       channel.ID,
		ChannelCode:     channel.Code,
		ChannelName:     channel.Name,
		RepayDay:        repayDay,
		FirstDueDate:    firstDueDate,
		Principal:       tr.Amount,
		TotalFee:        totalFee,
		Periods:         periods,
		FeeMode:         feeMode,
		FeeValue:        feeValue,
		Kind:            kind,
		AssetID:         assetID,
		PurchaseEntryID: &tr.ID,
		CategoryName:    tr.Category,
		Status:          "active",
		Note:            tr.Note,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := tx.Create(&plan).Error; err != nil {
		return nil, nil, err
	}

	items := make([]models.InstallmentItem, periods)
	for i := 0; i < periods; i++ {
		seq := i + 1
		dueDate, err := CalculateDueDate(firstDueDate, repayDay, seq)
		if err != nil {
			return nil, nil, err
		}
		items[i] = models.InstallmentItem{
			PlanID:        plan.ID,
			Seq:           seq,
			DueDate:       dueDate,
			PrincipalPart: principalParts[i],
			FeePart:       feeParts[i],
			Status:        "scheduled",
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}

	if err := tx.Create(&items).Error; err != nil {
		return nil, nil, err
	}

	tr.InstallmentPlanID = &plan.ID
	tr.Source = "installment_purchase"
	if err := tx.Model(tr).Updates(map[string]any{
		"installment_plan_id": plan.ID,
		"source":              "installment_purchase",
	}).Error; err != nil {
		return nil, nil, err
	}

	return &plan, items, nil
}

// CreateInstallmentPlanRequest 创建分期计划请求
type CreateInstallmentPlanRequest struct {
	ChannelID       uint    `json:"channel_id"`
	ChannelCode     string  `json:"channel_code"`
	Title           string  `json:"title"`
	Kind            string  `json:"kind"` // "asset" | "expense"
	Principal       int64   `json:"principal"`
	Periods         int     `json:"periods"`
	FeeMode         string  `json:"fee_mode"` // "none" | "fixed_per_period" | "rate_per_period" | "total_fee"
	FeeValue        int64   `json:"fee_value"`
	FeeRate         float64 `json:"fee_rate,omitempty"` // 兼容小数费率输入，如 0.005 代表 0.50%
	FirstDueDate    string  `json:"first_due_date"`
	RepayDay        int     `json:"repay_day"`
	AssetID         string  `json:"asset_id,omitempty"`
	CreateNewGear   bool    `json:"create_new_gear,omitempty"` // 类型 A 且录入新设备时为 true
	GearName        string  `json:"gear_name,omitempty"`
	GearSpec        string  `json:"gear_spec,omitempty"`
	GearCategory    string  `json:"gear_category,omitempty"`
	CategoryID      *uint   `json:"category_id,omitempty"`   // 类型 B 分类 ID
	CategoryName    string  `json:"category_name,omitempty"` // 类型 B 分类名称
	PurchaseDate    string  `json:"purchase_date,omitempty"` // 购买日期 YYYY-MM-DD
	Note            string  `json:"note,omitempty"`
}

// CreateInstallmentPlan 创建分期计划
func CreateInstallmentPlan(c *gin.Context) {
	var req CreateInstallmentPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效: "+err.Error())
		return
	}
	log.Printf("CreateInstallmentPlan received: %+v, kind: %q", req, req.Kind)

	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		Error(c, http.StatusBadRequest, "分期计划名称不能为空")
		return
	}
	if req.Principal <= 0 {
		Error(c, http.StatusBadRequest, "本金必须大于 0")
		return
	}
	if req.Periods <= 0 || req.Periods > 120 {
		Error(c, http.StatusBadRequest, "分期期数必须在 1 到 120 之间")
		return
	}
	if req.Kind == "" {
		req.Kind = "asset"
	}
	if req.Kind != "asset" && req.Kind != "expense" {
		Error(c, http.StatusBadRequest, "分期类型仅支持 'asset' (购入装备) 或 'expense' (一笔消费)")
		return
	}

	// 查找渠道
	var channel models.CreditChannel
	if req.ChannelID > 0 {
		if err := db.DB.First(&channel, "id = ?", req.ChannelID).Error; err != nil {
			Error(c, http.StatusBadRequest, "指定的信贷渠道不存在")
			return
		}
	} else if req.ChannelCode != "" {
		if err := db.DB.First(&channel, "code = ?", req.ChannelCode).Error; err != nil {
			Error(c, http.StatusBadRequest, "指定的信贷渠道不存在: "+req.ChannelCode)
			return
		}
	} else {
		// 默认取首个信贷渠道
		if err := db.DB.Order("id ASC").First(&channel).Error; err != nil {
			Error(c, http.StatusBadRequest, "尚未配置任何信贷渠道")
			return
		}
	}

	repayDay := req.RepayDay
	if repayDay < 1 || repayDay > 31 {
		repayDay = channel.RepaymentDay
	}
	if repayDay < 1 {
		repayDay = 10
	}

	// 若未指定首期还款日，按当前时间与还款日推算下一个还款日
	firstDueDate := strings.TrimSpace(req.FirstDueDate)
	if firstDueDate == "" {
		now := config.NowInBeijing()
		daysUntil := calculateDaysUntilRepayment(repayDay)
		nextRepayDate := now.AddDate(0, 0, daysUntil)
		firstDueDate = nextRepayDate.Format("2006-01-02")
	}

	feeMode := req.FeeMode
	if feeMode == "" {
		feeMode = "none"
	}
	feeValue := req.FeeValue
	if feeMode == "rate_per_period" && feeValue == 0 && req.FeeRate > 0 {
		// 将小数费率转化为基点 bps (例如 0.005 -> 50 bps)
		feeValue = int64(req.FeeRate*10000 + 0.5)
	}

	principalParts, feeParts, totalFee, err := SplitInstallmentPeriods(req.Principal, req.Periods, feeMode, feeValue)
	if err != nil {
		Error(c, http.StatusBadRequest, err.Error())
		return
	}

	// 联动建 gear 时先取器物互斥锁再开事务（锁序 profileGearMu → DB，与 CreateTransaction / ResolveWish 一致）
	if req.Kind == "asset" {
		profileGearMu.Lock()
		defer profileGearMu.Unlock()
	}

	dbTx := db.DB.Begin()
	defer dbTx.Rollback()

	now := config.NowInBeijing()
	purchaseDateStr := strings.TrimSpace(req.PurchaseDate)
	var purchaseOccurredAt time.Time
	if purchaseDateStr != "" {
		t, err := time.ParseInLocation("2006-01-02", purchaseDateStr, config.BeijingLocation)
		if err == nil {
			purchaseOccurredAt = t
		} else {
			purchaseOccurredAt = now
		}
	} else {
		purchaseOccurredAt = now
	}

	plan := models.InstallmentPlan{
		ChannelID:    channel.ID,
		ChannelCode:  channel.Code,
		ChannelName:  channel.Name,
		Title:        req.Title,
		Kind:         req.Kind,
		Principal:    req.Principal,
		Periods:      req.Periods,
		FeeMode:      feeMode,
		FeeValue:     feeValue,
		TotalFee:     totalFee,
		FirstDueDate: firstDueDate,
		RepayDay:     repayDay,
		AssetID:      req.AssetID,
		CategoryID:   req.CategoryID,
		CategoryName: req.CategoryName,
		Status:       "active",
		Note:         req.Note,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	// 类型 A：购入装备处理
	if req.Kind == "asset" {
		gearName := req.GearName
		if gearName == "" && req.Title != "" {
			gearName = req.Title
		}
		if (req.CreateNewGear || req.AssetID == "") && gearName != "" {
			gearID := fmt.Sprintf("gear_%d", now.UnixNano())
			gearCat := req.GearCategory
			if gearCat == "" {
				gearCat = "数码器材"
			}
			newGear := models.GearItem{
				ID:           gearID,
				Name:         gearName,
				Spec:         req.GearSpec,
				Category:     gearCat,
				Status:       "active",
				PriceCents:   req.Principal,
				PurchaseDate: purchaseOccurredAt.Format("2006-01-02"),
			}
			if err := mutateProfileGearLocked(dbTx, func(list []models.GearItem) ([]models.GearItem, error) {
				return append(list, newGear), nil
			}); err != nil {
				dbTx.Rollback()
				Error(c, http.StatusInternalServerError, "保存新装备资产失败: "+err.Error())
				return
			}
			plan.AssetID = gearID
		}
	}

	// 类型 B：一笔消费处理 (生成购买日支出流水，标记 source = 'installment_purchase'，不扣现金)
	if req.Kind == "expense" {
		cat := req.CategoryName
		if cat == "" {
			cat = "购物"
		}
		purchaseTx := models.Transaction{
			Amount:        req.Principal,
			Type:          "expense",
			Category:      cat,
			Title:         req.Title,
			PaymentMethod: channel.Name,
			OccurredAt:    purchaseOccurredAt,
			Source:        "installment_purchase",
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := dbTx.Create(&purchaseTx).Error; err != nil {
			dbTx.Rollback()
			Error(c, http.StatusInternalServerError, "生成消费账目失败: "+err.Error())
			return
		}
		plan.PurchaseEntryID = &purchaseTx.ID
	}

	if err := dbTx.Create(&plan).Error; err != nil {
		dbTx.Rollback()
		Error(c, http.StatusInternalServerError, "创建分期计划失败: "+err.Error())
		return
	}

	// 如果生成了购买流水，回填 plan_id
	if plan.PurchaseEntryID != nil {
		_ = dbTx.Model(&models.Transaction{}).Where("id = ?", *plan.PurchaseEntryID).
			Update("installment_plan_id", plan.ID).Error
	}

	// 生成全部期次记录
	items := make([]models.InstallmentItem, req.Periods)
	for i := 0; i < req.Periods; i++ {
		seq := i + 1
		dueDate, err := CalculateDueDate(firstDueDate, repayDay, seq)
		if err != nil {
			dbTx.Rollback()
			Error(c, http.StatusInternalServerError, "计算分期到期日失败: "+err.Error())
			return
		}
		items[i] = models.InstallmentItem{
			PlanID:        plan.ID,
			Seq:           seq,
			DueDate:       dueDate,
			PrincipalPart: principalParts[i],
			FeePart:       feeParts[i],
			Status:        "scheduled",
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}

	if err := dbTx.Create(&items).Error; err != nil {
		dbTx.Rollback()
		Error(c, http.StatusInternalServerError, "生成分期期次失败: "+err.Error())
		return
	}

	if err := dbTx.Commit().Error; err != nil {
		Error(c, http.StatusInternalServerError, "提交事务失败: "+err.Error())
		return
	}

	InvalidateStatsCache()
	Success(c, gin.H{
		"plan":  plan,
		"items": items,
	})
}

// InstallmentPlanItemSummary 分期计划列表项附加汇总信息
type InstallmentPlanItemSummary struct {
	models.InstallmentPlan
	RemainingPrincipal int64  `json:"remaining_principal"` // 剩余待还本金（分）
	RemainingFee       int64  `json:"remaining_fee"`       // 剩余手续费（分）
	RemainingTotal     int64  `json:"remaining_total"`     // 剩余应还总额（分）
	PaidPrincipal      int64  `json:"paid_principal"`      // 已还本金（分）
	PaidFee            int64  `json:"paid_fee"`            // 已还手续费（分）
	PaidTotal          int64  `json:"paid_total"`          // 已还总金额（分）
	PaidPeriods        int    `json:"paid_periods"`        // 已还期数
	NextDueDate        string `json:"next_due_date"`       // 下次到期日
	NextPrincipal      int64  `json:"next_principal"`      // 下次应还本金
	NextFee            int64  `json:"next_fee"`            // 下次应还手续费
	NextTotal          int64  `json:"next_total"`          // 下次应还总额
	IsOverdue          bool   `json:"is_overdue"`          // 是否有已逾期期次
	OverdueDays        int    `json:"overdue_days"`        // 最大逾期天数
}

// GetInstallmentPlans 查询分期计划列表
func GetInstallmentPlans(c *gin.Context) {
	status := c.Query("status")
	channelCode := c.Query("channel_code")

	query := db.DB.Model(&models.InstallmentPlan{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if channelCode != "" {
		query = query.Where("channel_code = ?", channelCode)
	}

	var plans []models.InstallmentPlan
	if err := query.Order("id DESC").Find(&plans).Error; err != nil {
		Error(c, http.StatusInternalServerError, "查询分期计划失败")
		return
	}

	todayStr := config.NowInBeijing().Format("2006-01-02")
	todayDate, _ := time.ParseInLocation("2006-01-02", todayStr, config.BeijingLocation)

	itemsByPlanID := make(map[uint][]models.InstallmentItem, len(plans))
	if len(plans) > 0 {
		planIDs := make([]uint, len(plans))
		for i, p := range plans {
			planIDs[i] = p.ID
		}
		var allItems []models.InstallmentItem
		db.DB.Where("plan_id IN ?", planIDs).Order("plan_id ASC, seq ASC").Find(&allItems)
		for _, it := range allItems {
			itemsByPlanID[it.PlanID] = append(itemsByPlanID[it.PlanID], it)
		}
	}

	results := make([]InstallmentPlanItemSummary, 0, len(plans))
	for _, plan := range plans {
		items := itemsByPlanID[plan.ID]

		summary := InstallmentPlanItemSummary{
			InstallmentPlan: plan,
		}

		firstScheduledFound := false
		maxOverdueDays := 0

		for _, it := range items {
			if it.Status == "paid" {
				summary.PaidPrincipal += it.PrincipalPart
				summary.PaidFee += it.FeePart
				summary.PaidTotal += it.PaidAmount
				summary.PaidPeriods++
			} else {
				summary.RemainingPrincipal += it.PrincipalPart
				summary.RemainingFee += it.FeePart
				summary.RemainingTotal += (it.PrincipalPart + it.FeePart)

				if !firstScheduledFound {
					firstScheduledFound = true
					summary.NextDueDate = it.DueDate
					summary.NextPrincipal = it.PrincipalPart
					summary.NextFee = it.FeePart
					summary.NextTotal = it.PrincipalPart + it.FeePart
				}

				// 检查逾期
				itemDueDate, err := time.ParseInLocation("2006-01-02", it.DueDate, config.BeijingLocation)
				if err == nil && itemDueDate.Before(todayDate) {
					summary.IsOverdue = true
					days := int(todayDate.Sub(itemDueDate).Hours() / 24)
					if days > maxOverdueDays {
						maxOverdueDays = days
					}
				}
			}
		}
		summary.OverdueDays = maxOverdueDays
		results = append(results, summary)
	}

	Success(c, results)
}

// GetInstallmentPlanDetail 获取分期计划详情与全部期次
func GetInstallmentPlanDetail(c *gin.Context) {
	id := c.Param("id")
	var plan models.InstallmentPlan
	if err := db.DB.First(&plan, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "分期计划不存在")
		return
	}

	var items []models.InstallmentItem
	if err := db.DB.Where("plan_id = ?", plan.ID).Order("seq ASC").Find(&items).Error; err != nil {
		Error(c, http.StatusInternalServerError, "查询分期明细失败")
		return
	}

	todayStr := config.NowInBeijing().Format("2006-01-02")
	todayDate, _ := time.ParseInLocation("2006-01-02", todayStr, config.BeijingLocation)

	var remainingPrincipal, remainingFee, paidPrincipal, paidFee int64
	var paidPeriods int
	var nextDueDate string
	var nextPrincipal, nextFee int64
	firstScheduled := false
	isOverdue := false
	maxOverdueDays := 0

	for _, it := range items {
		if it.Status == "paid" {
			paidPrincipal += it.PrincipalPart
			paidFee += it.FeePart
			paidPeriods++
		} else {
			remainingPrincipal += it.PrincipalPart
			remainingFee += it.FeePart
			if !firstScheduled {
				firstScheduled = true
				nextDueDate = it.DueDate
				nextPrincipal = it.PrincipalPart
				nextFee = it.FeePart
			}
			dueDate, err := time.ParseInLocation("2006-01-02", it.DueDate, config.BeijingLocation)
			if err == nil && dueDate.Before(todayDate) {
				isOverdue = true
				days := int(todayDate.Sub(dueDate).Hours() / 24)
				if days > maxOverdueDays {
					maxOverdueDays = days
				}
			}
		}
	}

	Success(c, gin.H{
		"plan":                plan,
		"items":               items,
		"remaining_principal": remainingPrincipal,
		"remaining_fee":       remainingFee,
		"remaining_total":     remainingPrincipal + remainingFee,
		"paid_principal":      paidPrincipal,
		"paid_fee":            paidFee,
		"paid_total":          paidPrincipal + paidFee,
		"paid_periods":        paidPeriods,
		"next_due_date":       nextDueDate,
		"next_principal":      nextPrincipal,
		"next_fee":            nextFee,
		"is_overdue":          isOverdue,
		"overdue_days":        maxOverdueDays,
	})
}

// UpdateInstallmentPlanRequest 编辑分期计划请求
type UpdateInstallmentPlanRequest struct {
	Title string `json:"title"`
	Note  string `json:"note"`
}

// UpdateInstallmentPlan 编辑分期计划信息 (若已有还款记录，本金与期数不可改，仅允许修改标题与备注)
func UpdateInstallmentPlan(c *gin.Context) {
	id := c.Param("id")
	var plan models.InstallmentPlan
	if err := db.DB.First(&plan, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "分期计划不存在")
		return
	}

	var req UpdateInstallmentPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	if title := strings.TrimSpace(req.Title); title != "" {
		plan.Title = title
	}
	plan.Note = req.Note
	plan.UpdatedAt = config.NowInBeijing()

	if err := db.DB.Save(&plan).Error; err != nil {
		Error(c, http.StatusInternalServerError, "保存分期计划失败")
		return
	}

	Success(c, plan)
}

// UpdateInstallmentItemRequest 编辑期次请求 (支持修改单期到期日)
type UpdateInstallmentItemRequest struct {
	DueDate string `json:"due_date"`
}

// UpdateInstallmentItem 编辑单期到期日 (RC-140)
func UpdateInstallmentItem(c *gin.Context) {
	id := c.Param("id")
	var item models.InstallmentItem
	if err := db.DB.First(&item, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "期次记录不存在")
		return
	}
	if item.Status == "paid" {
		Error(c, http.StatusBadRequest, "已还期次不可修改到期日")
		return
	}
	var req UpdateInstallmentItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}
	dueDate := strings.TrimSpace(req.DueDate)
	if dueDate != "" {
		if _, err := time.Parse("2006-01-02", dueDate); err != nil {
			Error(c, http.StatusBadRequest, "到期日格式必须为 YYYY-MM-DD")
			return
		}
		item.DueDate = dueDate
		item.UpdatedAt = config.NowInBeijing()
		if err := db.DB.Save(&item).Error; err != nil {
			Error(c, http.StatusInternalServerError, "保存期次失败")
			return
		}
	}
	Success(c, item)
}

// PayInstallmentItemRequest 标记已还请求
type PayInstallmentItemRequest struct {
	PaidAmount *int64 `json:"paid_amount"` // 可选，默认为应还金额 (principal_part + fee_part)
	PayAccount string `json:"pay_account"` // 付款账户/渠道
	PaidAt     string `json:"paid_at"`     // 支付时间 ISO 或 YYYY-MM-DD
}

// PayInstallmentItem 标记某期已还 (RC-140)
func PayInstallmentItem(c *gin.Context) {
	id := c.Param("id")
	var item models.InstallmentItem
	if err := db.DB.First(&item, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "期次记录不存在")
		return
	}

	if item.Status == "paid" {
		Error(c, http.StatusBadRequest, "该期次已标记还款，不得重复入账")
		return
	}

	var plan models.InstallmentPlan
	if err := db.DB.First(&plan, "id = ?", item.PlanID).Error; err != nil {
		Error(c, http.StatusNotFound, "所属分期计划不存在")
		return
	}

	var req PayInstallmentItemRequest
	_ = c.ShouldBindJSON(&req)

	// 应还本金与手续费
	expectedTotal := item.PrincipalPart + item.FeePart
	paidAmount := expectedTotal
	if req.PaidAmount != nil && *req.PaidAmount > 0 {
		paidAmount = *req.PaidAmount
	}

	// 规则 10：实付金额不得低于本期本金，超出部分计为手续费
	if paidAmount < item.PrincipalPart {
		Error(c, http.StatusBadRequest, fmt.Sprintf("实付金额（¥%.2f）不得低于本期应还本金（¥%.2f）",
			float64(paidAmount)/100, float64(item.PrincipalPart)/100))
		return
	}

	actualFee := paidAmount - item.PrincipalPart
	payAccount := strings.TrimSpace(req.PayAccount)
	if payAccount == "" {
		payAccount = "银行卡"
	}

	now := config.NowInBeijing()
	paidAt := now
	if paidAtStr := strings.TrimSpace(req.PaidAt); paidAtStr != "" {
		if t, err := time.Parse(time.RFC3339, paidAtStr); err == nil {
			paidAt = t.In(config.BeijingLocation)
		} else if t, err := time.ParseInLocation("2006-01-02", paidAtStr, config.BeijingLocation); err == nil {
			paidAt = t
		}
	}

	dbTx := db.DB.Begin()
	defer dbTx.Rollback()

	// 在事务内原子抢占 scheduled 状态，防止并发重复点击产生双份还款流水
	claimRes := dbTx.Model(&models.InstallmentItem{}).
		Where("id = ? AND status = ?", item.ID, "scheduled").
		Updates(map[string]any{
			"status":      "paid",
			"paid_at":     &paidAt,
			"paid_amount": paidAmount,
			"pay_account": payAccount,
			"updated_at":  now,
		})
	if claimRes.Error != nil {
		Error(c, http.StatusInternalServerError, "更新期次状态失败: "+claimRes.Error.Error())
		return
	}
	if claimRes.RowsAffected == 0 {
		Error(c, http.StatusBadRequest, "该期次已标记还款，不得重复入账")
		return
	}

	// 生成还款支出账目流水
	repayTx := models.Transaction{
		Amount:            paidAmount,
		Type:              "expense",
		Category:          "利息手续费",
		Title:             fmt.Sprintf("%s · 第 %d/%d 期", plan.Title, item.Seq, plan.Periods),
		PaymentMethod:     payAccount,
		OccurredAt:        paidAt,
		Source:            "installment_repay",
		InstallmentPlanID: &plan.ID,
		InstallmentItemID: &item.ID,
		PrincipalAmount:   item.PrincipalPart,
		FeeAmount:         actualFee,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := dbTx.Create(&repayTx).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建还款流水失败: "+err.Error())
		return
	}

	if err := dbTx.Model(&models.InstallmentItem{}).
		Where("id = ?", item.ID).
		Update("transaction_id", repayTx.ID).Error; err != nil {
		Error(c, http.StatusInternalServerError, "绑定还款流水失败: "+err.Error())
		return
	}

	item.Status = "paid"
	item.PaidAt = &paidAt
	item.PaidAmount = paidAmount
	item.PayAccount = payAccount
	item.TransactionID = &repayTx.ID
	item.UpdatedAt = now

	// 检查是否全期已还完
	var remainingCount int64
	dbTx.Model(&models.InstallmentItem{}).Where("plan_id = ? AND status = 'scheduled'", plan.ID).Count(&remainingCount)
	if remainingCount == 0 {
		plan.Status = "settled"
		plan.UpdatedAt = now
		_ = dbTx.Save(&plan).Error
	}

	if err := dbTx.Commit().Error; err != nil {
		Error(c, http.StatusInternalServerError, "提交事务失败: "+err.Error())
		return
	}

	InvalidateStatsCache()
	Success(c, gin.H{
		"item":        item,
		"transaction": repayTx,
		"plan_status": plan.Status,
	})
}

// UnpayInstallmentItem 撤销某期已还
func UnpayInstallmentItem(c *gin.Context) {
	id := c.Param("id")
	var item models.InstallmentItem
	if err := db.DB.First(&item, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "期次记录不存在")
		return
	}

	if item.Status != "paid" {
		Error(c, http.StatusBadRequest, "该期次尚未还款，无需撤销")
		return
	}

	var plan models.InstallmentPlan
	if err := db.DB.First(&plan, "id = ?", item.PlanID).Error; err != nil {
		Error(c, http.StatusNotFound, "所属分期计划不存在")
		return
	}

	dbTx := db.DB.Begin()
	defer dbTx.Rollback()

	// 软删除关联的流水记录
	if item.TransactionID != nil {
		_ = dbTx.Delete(&models.Transaction{}, *item.TransactionID).Error
	}

	now := config.NowInBeijing()
	item.Status = "scheduled"
	item.PaidAt = nil
	item.PaidAmount = 0
	item.PayAccount = ""
	item.TransactionID = nil
	item.UpdatedAt = now

	if err := dbTx.Save(&item).Error; err != nil {
		dbTx.Rollback()
		Error(c, http.StatusInternalServerError, "回滚期次失败: "+err.Error())
		return
	}

	if plan.Status == "settled" {
		plan.Status = "active"
		plan.UpdatedAt = now
		_ = dbTx.Save(&plan).Error
	}

	if err := dbTx.Commit().Error; err != nil {
		Error(c, http.StatusInternalServerError, "提交事务失败: "+err.Error())
		return
	}

	InvalidateStatsCache()
	Success(c, gin.H{
		"item":        item,
		"plan_status": plan.Status,
		"unpaid":      true,
	})
}

// SettleInstallmentPlanRequest 提前结清请求
type SettleInstallmentPlanRequest struct {
	PaidAmount int64  `json:"paid_amount"` // 实际支付总额 (分)
	PayAccount string `json:"pay_account"` // 付款账户
	PaidAt     string `json:"paid_at"`     // 支付时间
}

// SettleInstallmentPlan 提前结清分期计划
func SettleInstallmentPlan(c *gin.Context) {
	id := c.Param("id")
	var plan models.InstallmentPlan
	if err := db.DB.First(&plan, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "分期计划不存在")
		return
	}

	if plan.Status == "settled" {
		Error(c, http.StatusBadRequest, "该分期计划已经结清")
		return
	}
	if plan.Status == "cancelled" {
		Error(c, http.StatusBadRequest, "该分期计划已取消，无法提前结清")
		return
	}

	var req SettleInstallmentPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	// 查询所有剩余未还期次
	var scheduledItems []models.InstallmentItem
	if err := db.DB.Where("plan_id = ? AND status = 'scheduled'", plan.ID).Order("seq ASC").Find(&scheduledItems).Error; err != nil {
		Error(c, http.StatusInternalServerError, "查询未还期次失败")
		return
	}

	if len(scheduledItems) == 0 {
		plan.Status = "settled"
		_ = db.DB.Save(&plan).Error
		Success(c, gin.H{"settled": true, "plan": plan})
		return
	}

	var remainingPrincipal int64
	for _, it := range scheduledItems {
		remainingPrincipal += it.PrincipalPart
	}

	// 规则 9：实付金额不得低于剩余本金，差额记为手续费支出 (<0 则拒绝)
	if req.PaidAmount < remainingPrincipal {
		Error(c, http.StatusBadRequest, fmt.Sprintf("实付金额（¥%.2f）不得低于剩余本金（¥%.2f）",
			float64(req.PaidAmount)/100, float64(remainingPrincipal)/100))
		return
	}

	settleFee := req.PaidAmount - remainingPrincipal
	payAccount := strings.TrimSpace(req.PayAccount)
	if payAccount == "" {
		payAccount = "银行卡"
	}

	now := config.NowInBeijing()
	paidAt := now
	if paidAtStr := strings.TrimSpace(req.PaidAt); paidAtStr != "" {
		if t, err := time.Parse(time.RFC3339, paidAtStr); err == nil {
			paidAt = t.In(config.BeijingLocation)
		} else if t, err := time.ParseInLocation("2006-01-02", paidAtStr, config.BeijingLocation); err == nil {
			paidAt = t
		}
	}

	dbTx := db.DB.Begin()
	defer dbTx.Rollback()

	// 事务内原子抢占计划状态，防止并发重复结清产生双份结清流水
	claimPlan := dbTx.Model(&models.InstallmentPlan{}).
		Where("id = ? AND status NOT IN ?", plan.ID, []string{"settled", "cancelled"}).
		Updates(map[string]any{
			"status":     "settled",
			"updated_at": now,
		})
	if claimPlan.Error != nil {
		Error(c, http.StatusInternalServerError, "更新计划状态失败: "+claimPlan.Error.Error())
		return
	}
	if claimPlan.RowsAffected == 0 {
		Error(c, http.StatusBadRequest, "该分期计划已经结清或已取消")
		return
	}

	// 生成结清流水
	repayTx := models.Transaction{
		Amount:            req.PaidAmount,
		Type:              "expense",
		Category:          "利息手续费",
		Title:             fmt.Sprintf("%s · 提前结清", plan.Title),
		PaymentMethod:     payAccount,
		OccurredAt:        paidAt,
		Source:            "installment_repay",
		InstallmentPlanID: &plan.ID,
		PrincipalAmount:   remainingPrincipal,
		FeeAmount:         settleFee,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := dbTx.Create(&repayTx).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建结清流水失败: "+err.Error())
		return
	}

	// 所有未还期次标记为已还
	for i := range scheduledItems {
		it := &scheduledItems[i]
		it.Status = "paid"
		it.PaidAt = &paidAt
		it.PaidAmount = it.PrincipalPart
		it.PayAccount = payAccount
		it.TransactionID = &repayTx.ID
		it.UpdatedAt = now
		if err := dbTx.Save(it).Error; err != nil {
			Error(c, http.StatusInternalServerError, "更新期次状态失败: "+err.Error())
			return
		}
	}

	plan.Status = "settled"
	plan.UpdatedAt = now

	if err := dbTx.Commit().Error; err != nil {
		Error(c, http.StatusInternalServerError, "提交事务失败: "+err.Error())
		return
	}

	InvalidateStatsCache()
	Success(c, gin.H{
		"settled":         true,
		"plan":            plan,
		"transaction":     repayTx,
		"settled_fee":     settleFee,
		"settled_periods": len(scheduledItems),
	})
}

// DeleteInstallmentPlan 取消或删除分期计划
// 保护约束：已有还款记录的计划不可删除，只能提前结清
func DeleteInstallmentPlan(c *gin.Context) {
	id := c.Param("id")
	var plan models.InstallmentPlan
	if err := db.DB.First(&plan, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "分期计划不存在")
		return
	}

	var paidCount int64
	db.DB.Model(&models.InstallmentItem{}).Where("plan_id = ? AND status = 'paid'", plan.ID).Count(&paidCount)
	if paidCount > 0 {
		Error(c, http.StatusBadRequest, "已有还款记录的计划不可删除，如已还完请保持结清，或使用提前结清")
		return
	}

	dbTx := db.DB.Begin()
	defer dbTx.Rollback()

	// 清理该计划的所有期次
	_ = dbTx.Where("plan_id = ?", plan.ID).Delete(&models.InstallmentItem{}).Error

	// 若为类型 B 且生成过初始购买支出，同步清理该笔消费流水
	if plan.PurchaseEntryID != nil {
		_ = dbTx.Delete(&models.Transaction{}, *plan.PurchaseEntryID).Error
	}

	// 软删除计划本身
	if err := dbTx.Delete(&plan).Error; err != nil {
		dbTx.Rollback()
		Error(c, http.StatusInternalServerError, "删除计划失败: "+err.Error())
		return
	}

	if err := dbTx.Commit().Error; err != nil {
		Error(c, http.StatusInternalServerError, "提交事务失败: "+err.Error())
		return
	}

	InvalidateStatsCache()
	Success(c, gin.H{"id": plan.ID, "deleted": true})
}

// InstallmentSummaryResponse 负债大盘汇总响应
type InstallmentSummaryResponse struct {
	TotalRemainingPrincipal int64  `json:"total_remaining_principal"` // 待还总本金（分）
	ThisMonthPaid           int64  `json:"this_month_paid"`           // 本月已还总金额（分）
	ThisMonthScheduled      int64  `json:"this_month_scheduled"`      // 本月待还总金额（分）
	NearestDueDate          string `json:"nearest_due_date"`          // 最近还款日
	OverdueCount            int    `json:"overdue_count"`             // 逾期期数
	OverdueAmount           int64  `json:"overdue_amount"`            // 逾期金额（分）
}

// GetInstallmentSummary 负债大盘汇总
func GetInstallmentSummary(c *gin.Context) {
	now := config.NowInBeijing()
	curYear, curMonth, _ := now.Date()
	startOfMonth := fmt.Sprintf("%04d-%02d-01", curYear, curMonth)
	nextMonthYear := curYear
	nextMonthVal := int(curMonth) + 1
	if nextMonthVal > 12 {
		nextMonthVal = 1
		nextMonthYear++
	}
	endOfMonth := fmt.Sprintf("%04d-%02d-01", nextMonthYear, nextMonthVal)
	todayStr := now.Format("2006-01-02")
	todayDate, _ := time.ParseInLocation("2006-01-02", todayStr, config.BeijingLocation)

	var totalRemainingPrincipal int64
	db.DB.Model(&models.InstallmentItem{}).
		Where("status = 'scheduled'").
		Select("COALESCE(SUM(principal_part), 0)").
		Scan(&totalRemainingPrincipal)

	// 本月已还：paid_at 在本月内的支付金额之和
	startT, _ := time.ParseInLocation("2006-01-02", startOfMonth, config.BeijingLocation)
	endT, _ := time.ParseInLocation("2006-01-02", endOfMonth, config.BeijingLocation)
	var thisMonthPaid int64
	db.DB.Model(&models.InstallmentItem{}).
		Where("status = 'paid' AND paid_at >= ? AND paid_at < ?", startT, endT).
		Select("COALESCE(SUM(paid_amount), 0)").
		Scan(&thisMonthPaid)

	// 本月待还：due_date 在当月且 status = 'scheduled'
	var thisMonthScheduled int64
	db.DB.Model(&models.InstallmentItem{}).
		Where("status = 'scheduled' AND due_date >= ? AND due_date < ?", startOfMonth, endOfMonth).
		Select("COALESCE(SUM(principal_part + fee_part), 0)").
		Scan(&thisMonthScheduled)

	// 逾期统计与最近还款日
	var scheduledItems []models.InstallmentItem
	db.DB.Where("status = 'scheduled'").Order("due_date ASC").Find(&scheduledItems)

	nearestDueDate := ""
	overdueCount := 0
	var overdueAmount int64

	for _, it := range scheduledItems {
		if nearestDueDate == "" {
			nearestDueDate = it.DueDate
		}
		itemDue, err := time.ParseInLocation("2006-01-02", it.DueDate, config.BeijingLocation)
		if err == nil && itemDue.Before(todayDate) {
			overdueCount++
			overdueAmount += (it.PrincipalPart + it.FeePart)
		}
	}

	Success(c, InstallmentSummaryResponse{
		TotalRemainingPrincipal: totalRemainingPrincipal,
		ThisMonthPaid:           thisMonthPaid,
		ThisMonthScheduled:      thisMonthScheduled,
		NearestDueDate:          nearestDueDate,
		OverdueCount:            overdueCount,
		OverdueAmount:           overdueAmount,
	})
}

// UpcomingInstallmentItemResponse 待还期次展示条目
type UpcomingInstallmentItemResponse struct {
	ItemID        uint   `json:"item_id"`
	PlanID        uint   `json:"plan_id"`
	PlanTitle     string `json:"plan_title"`
	ChannelName   string `json:"channel_name"`
	Seq           int    `json:"seq"`
	TotalPeriods  int    `json:"total_periods"`
	DueDate       string `json:"due_date"`
	PrincipalPart int64  `json:"principal_part"`
	FeePart       int64  `json:"fee_part"`
	TotalPart     int64  `json:"total_part"`
	IsOverdue     bool   `json:"is_overdue"`
	OverdueDays   int    `json:"overdue_days"`
}

// GetUpcomingInstallmentItems 获取本月待还与已逾期的期次列表 (供首页弹层直接一键还款)
func GetUpcomingInstallmentItems(c *gin.Context) {
	now := config.NowInBeijing()
	curYear, curMonth, _ := now.Date()
	nextMonthYear := curYear
	nextMonthVal := int(curMonth) + 1
	if nextMonthVal > 12 {
		nextMonthVal = 1
		nextMonthYear++
	}
	endOfMonth := fmt.Sprintf("%04d-%02d-01", nextMonthYear, nextMonthVal)
	todayStr := now.Format("2006-01-02")
	todayDate, _ := time.ParseInLocation("2006-01-02", todayStr, config.BeijingLocation)

	// 查询所有 scheduled 且 due_date < endOfMonth 的期次 (含历史逾期与本月待还)
	var items []models.InstallmentItem
	db.DB.Where("status = 'scheduled' AND due_date < ?", endOfMonth).
		Order("due_date ASC, seq ASC").
		Find(&items)

	planMap := make(map[uint]models.InstallmentPlan)
	if len(items) > 0 {
		planIDSet := make(map[uint]struct{}, len(items))
		planIDs := make([]uint, 0, len(items))
		for _, it := range items {
			if _, ok := planIDSet[it.PlanID]; !ok {
				planIDSet[it.PlanID] = struct{}{}
				planIDs = append(planIDs, it.PlanID)
			}
		}
		var plans []models.InstallmentPlan
		db.DB.Where("id IN ?", planIDs).Find(&plans)
		for _, p := range plans {
			planMap[p.ID] = p
		}
	}
	results := make([]UpcomingInstallmentItemResponse, 0, len(items))

	for _, it := range items {
		plan := planMap[it.PlanID]

		itemDue, _ := time.ParseInLocation("2006-01-02", it.DueDate, config.BeijingLocation)
		isOverdue := itemDue.Before(todayDate)
		overdueDays := 0
		if isOverdue {
			overdueDays = int(todayDate.Sub(itemDue).Hours() / 24)
		}

		results = append(results, UpcomingInstallmentItemResponse{
			ItemID:        it.ID,
			PlanID:        it.PlanID,
			PlanTitle:     plan.Title,
			ChannelName:   plan.ChannelName,
			Seq:           it.Seq,
			TotalPeriods:  plan.Periods,
			DueDate:       it.DueDate,
			PrincipalPart: it.PrincipalPart,
			FeePart:       it.FeePart,
			TotalPart:     it.PrincipalPart + it.FeePart,
			IsOverdue:     isOverdue,
			OverdueDays:   overdueDays,
		})
	}

	Success(c, results)
}
