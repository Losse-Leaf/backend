package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// validTxTypes 记账 type 白名单：所有汇总口径只认 expense/income，非法值直接 400。
var validTxTypes = map[string]bool{
	"expense": true,
	"income":  true,
}

type TransactionRequest struct {
	Amount                  int64  `json:"amount" binding:"required"` // 分
	Type                    string `json:"type"`                      // income, expense
	Category                string `json:"category" binding:"required"`
	Title                   string `json:"title"`
	Note                    string `json:"note"`
	PaymentMethod           string `json:"payment_method"`
	WishID                  *uint  `json:"wish_id"`
	FootprintID             *uint  `json:"footprint_id"`
	CityCode                string `json:"city_code"`
	OccurredAt              string `json:"occurred_at"`
	IsInstallment           *bool  `json:"is_installment"`
	ClearInstallment        bool   `json:"clear_installment"`
	InstallmentChannelCode  string `json:"installment_channel_code"`
	InstallmentTotalAmount  int64  `json:"installment_total_amount"`
	InstallmentMonths       int    `json:"installment_months"`
	InstallmentMonthlyPay   int64  `json:"installment_monthly_pay"`
	InstallmentRemainMonths int    `json:"installment_remain_months"`
	RepaymentDay            int    `json:"repayment_day"`
	InstallmentFeeMode      string `json:"installment_fee_mode"`
	InstallmentFeeValue     int64  `json:"installment_fee_value"`
	FirstDueDate            string `json:"first_due_date"`
	LinkGearAsset           bool   `json:"link_gear_asset"`
	GearCategory            string `json:"gear_category"`
	GearSpec                string `json:"gear_spec"`
	ClearCity               bool   `json:"clear_city"`
	ClientRequestID         string `json:"client_request_id"`
}

// CreateTransaction 记一笔 (支持分期付款与随身设备库双向绑定 RC-53)
func CreateTransaction(c *gin.Context) {
	var req TransactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请填写金额、类型(支出/收入)与分类")
		return
	}
	if req.Amount <= 0 {
		Error(c, http.StatusBadRequest, "金额必须大于 0")
		return
	}

	occurred, err := ParseFlexTime(req.OccurredAt)
	if err != nil && strings.TrimSpace(req.OccurredAt) != "" {
		Error(c, http.StatusBadRequest, "发生时间格式无法识别")
		return
	}
	cleanCityCode, _, _ := ResolveCityCodeAndName(req.CityCode, "", "")

	clientRequestID := strings.TrimSpace(req.ClientRequestID)
	if existing, deleted, ok := findIdempotentReplay[models.Transaction](clientRequestID, func(t *models.Transaction) (uint, bool) {
		return t.ID, t.DeletedAt.Valid
	}); ok {
		replyIdempotentHit(c, existing, deleted, nil)
		return
	}

	txType := strings.TrimSpace(req.Type)
	if txType == "" {
		txType = "expense"
	}
	if !validTxTypes[txType] {
		Error(c, http.StatusBadRequest, "type 仅支持 expense / income")
		return
	}

	if txType == "income" {
		if (req.IsInstallment != nil && *req.IsInstallment) || req.InstallmentChannelCode != "" ||
			req.InstallmentTotalAmount > 0 || req.InstallmentMonths > 0 || req.InstallmentMonthlyPay > 0 ||
			req.InstallmentRemainMonths > 0 || req.RepaymentDay > 0 {
			Error(c, http.StatusBadRequest, "分期仅支持支出类型")
			return
		}
	}

	if !transactionCategoryAllowed(txType, req.Category) {
		Error(c, http.StatusBadRequest, "分类不存在于分类字典")
		return
	}

	if req.IsInstallment != nil && *req.IsInstallment {
		if req.InstallmentMonths > installmentMonthsLimit {
			Error(c, http.StatusBadRequest, "分期期数上限为 60 期")
			return
		}
		if !InstallmentChannelExists(nil, req.InstallmentChannelCode) {
			Error(c, http.StatusBadRequest, "分期渠道不存在")
			return
		}
		if req.InstallmentTotalAmount > 0 && req.InstallmentTotalAmount != req.Amount {
			Error(c, http.StatusBadRequest, "分期总额必须与账单金额一致")
			return
		}
	}

	if req.WishID != nil && *req.WishID == 0 {
		req.WishID = nil
	}
	if req.FootprintID != nil && *req.FootprintID == 0 {
		req.FootprintID = nil
	}
	if req.WishID != nil && *req.WishID > 0 {
		var wishCount int64
		db.DB.Unscoped().Model(&models.Wish{}).Where("id = ?", *req.WishID).Count(&wishCount)
		if wishCount == 0 {
			Error(c, http.StatusBadRequest, "关联的心愿不存在")
			return
		}
	}
	if req.FootprintID != nil && *req.FootprintID > 0 {
		var fpCount int64
		db.DB.Unscoped().Model(&models.Footprint{}).Where("id = ?", *req.FootprintID).Count(&fpCount)
		if fpCount == 0 {
			Error(c, http.StatusBadRequest, "关联的旅程不存在")
			return
		}
	}
	if req.WishID != nil && req.FootprintID != nil {
		Error(c, http.StatusBadRequest, "心愿与足迹只能二选一")
		return
	}

	record := models.Transaction{
		Amount:        req.Amount,
		Type:          txType,
		Category:      strings.TrimSpace(req.Category),
		Title:         strings.TrimSpace(req.Title),
		Note:          strings.TrimSpace(req.Note),
		PaymentMethod: strings.TrimSpace(req.PaymentMethod),
		WishID:        req.WishID,
		FootprintID:   req.FootprintID,
		CityCode:      cleanCityCode,
		OccurredAt:    occurred,
	}
	if clientRequestID != "" {
		record.ClientRequestID = &clientRequestID
	}

	autoLinkTransactionFootprint(&record)

	if req.IsInstallment != nil && *req.IsInstallment {
		AssembleInstallmentTransaction(
			&record,
			true,
			req.InstallmentChannelCode,
			req.InstallmentTotalAmount,
			req.Amount,
			req.InstallmentMonths,
			req.InstallmentMonthlyPay,
			req.InstallmentRemainMonths,
			req.RepaymentDay,
			db.DB,
		)
	}

	linkGear := req.LinkGearAsset
	if linkGear {
		profileGearMu.Lock()
		defer profileGearMu.Unlock()
	}

	err = db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}

		var newGearID string
		if linkGear {
			gearCategory := strings.TrimSpace(req.GearCategory)
			if gearCategory == "" {
				gearCategory = record.Category
			}
			createdGear, err := appendGearItemTx(tx, models.GearItem{
				Name:         record.Title,
				Spec:         req.GearSpec,
				Category:     gearCategory,
				Status:       "active",
				PriceCents:   record.Amount,
				PurchaseDate: record.OccurredAt.In(config.BeijingLocation).Format("2006-01-02"),
				Note:         record.Note,
			}, record.OccurredAt, false, false)
			if err != nil {
				return err
			}
			newGearID = createdGear.ID
		}

		if req.IsInstallment != nil && *req.IsInstallment && record.Type == "expense" {
			kind := "expense"
			if linkGear {
				kind = "asset"
			}
			_, _, err := CreatePlanForTransactionTx(
				tx,
				&record,
				req.InstallmentMonths,
				req.InstallmentChannelCode,
				req.InstallmentFeeMode,
				req.InstallmentFeeValue,
				req.FirstDueDate,
				req.RepaymentDay,
				kind,
				newGearID,
			)
			if err != nil {
				return err
			}
		}

		if req.WishID != nil && *req.WishID > 0 {
			var wish models.Wish
			if err := tx.First(&wish, *req.WishID).Error; err == nil && wish.Status == "wanting" {
				wish.Status = "fulfilled"
				occ := record.OccurredAt
				wish.FulfilledAt = &occ
				wish.TransactionID = &record.ID
				if wish.ActualCost == 0 {
					wish.ActualCost = record.Amount
				}
				if err := tx.Save(&wish).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		if existing, deleted, ok := findIdempotentReplay[models.Transaction](clientRequestID, func(t *models.Transaction) (uint, bool) {
			return t.ID, t.DeletedAt.Valid
		}); ok {
			replyIdempotentHit(c, existing, deleted, nil)
			return
		}
		Error(c, http.StatusInternalServerError, "记账失败: "+err.Error())
		return
	}

	if record.FootprintID != nil {
		touchFootprintTimestamps(db.DB, *record.FootprintID)
	}
	InvalidateStatsCache()

	Success(c, record)
}

// GetTransactions 列表查询账目 (支持按年、按月、增量游标与 ETag 缓存协商 BE-05)
func GetTransactions(c *gin.Context) {
	yearStr := c.Query("year")
	monthStr := c.Query("month")
	sinceStr := c.Query("since")
	includeDeleted := c.Query("include_deleted") == "true" || sinceStr != ""
	if !validateSyncSince(c, sinceStr) || !allowDeletedScope(c, includeDeleted) {
		return
	}

	dbScope := db.DB
	if includeDeleted {
		dbScope = db.DB.Unscoped()
	}

	query := dbScope.Model(&models.Transaction{}).Order("occurred_at DESC, id DESC")
	query = applySyncSince(query, &models.Transaction{}, sinceStr)

	if yearStr != "" {
		y, err := strconv.Atoi(yearStr)
		if err == nil {
			if monthStr != "" {
				m, err := strconv.Atoi(monthStr)
				if err == nil {
					start := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, config.BeijingLocation)
					end := start.AddDate(0, 1, 0)
					query = query.Where("occurred_at >= ? AND occurred_at < ?", start, end)
				}
			} else {
				start := time.Date(y, 1, 1, 0, 0, 0, 0, config.BeijingLocation)
				end := start.AddDate(1, 0, 0)
				query = query.Where("occurred_at >= ? AND occurred_at < ?", start, end)
			}
		}
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "500"))
	if limit > 1000 {
		limit = 1000
	}
	if limit > 0 {
		query = query.Limit(limit)
	}
	offset, _ := strconv.Atoi(c.Query("offset"))
	if offset > 0 {
		query = query.Offset(offset)
	}

	agg, err := aggregateListQuery(query)
	if err != nil {
		Error(c, http.StatusInternalServerError, "获取记账列表失败")
		return
	}
	if sinceStr == "" && negotiateListCache(c, listETag(c, agg, "")) {
		return
	}

	var txs []models.Transaction
	if err := query.Find(&txs).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取记账列表失败")
		return
	}

	if offset == 0 && sinceStr != "" {
		if err := injectTombstones(&txs, TombstoneEntityTransaction, sinceStr,
			func(t models.Transaction) uint { return t.ID },
			func(id uint, deletedAt time.Time) models.Transaction {
				return models.Transaction{
					ID:        id,
					CreatedAt: deletedAt,
					UpdatedAt: deletedAt,
					DeletedAt: gorm.DeletedAt{Time: deletedAt, Valid: true},
				}
			},
		); err != nil {
			Error(c, http.StatusInternalServerError, "获取记账列表失败")
			return
		}
	}

	now := config.NowInBeijing()
	for i := range txs {
		if txs[i].IsInstallment {
			_, autoRemain := CalculateElapsedInstallmentMonths(txs[i].OccurredAt, txs[i].InstallmentMonths, txs[i].RepaymentDay, now)
			if txs[i].InstallmentRemainMonths > autoRemain {
				txs[i].InstallmentRemainMonths = autoRemain
			}
		}
	}

	setSyncCursor(c, computeSyncCursor(txs, func(t models.Transaction) (time.Time, gorm.DeletedAt) {
		return t.UpdatedAt, t.DeletedAt
	}))

	Success(c, txs)
}

// GetTransactionSummary 月度记账统计
func GetTransactionSummary(c *gin.Context) {
	nowBJ := config.NowInBeijing()
	yearStr := c.DefaultQuery("year", fmt.Sprintf("%04d", nowBJ.Year()))
	monthStr := c.DefaultQuery("month", fmt.Sprintf("%02d", int(nowBJ.Month())))

	y, errY := strconv.Atoi(yearStr)
	m, errM := strconv.Atoi(monthStr)
	if errY != nil || errM != nil || y < 1900 || y > 2999 || m < 1 || m > 12 {
		Error(c, http.StatusBadRequest, "年月参数无效")
		return
	}

	start := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, config.BeijingLocation)
	end := start.AddDate(0, 1, 0)

	var txs []models.Transaction
	db.DB.Select("type, category, amount, source, fee_amount").Where("occurred_at >= ? AND occurred_at < ?", start, end).Find(&txs)

	var totalExpense int64
	var totalIncome int64
	categoryBreakdown := make(map[string]int64)

	for _, tx := range txs {
		if tx.Type == "expense" {
			expensePart := tx.Amount
			cat := tx.Category
			if tx.Source == "installment_repay" {
				expensePart = tx.FeeAmount
				cat = "利息手续费"
			}
			if expensePart > 0 {
				totalExpense += expensePart
				categoryBreakdown[cat] += expensePart
			}
		} else if tx.Type == "income" {
			totalIncome += tx.Amount
		}
	}

	Success(c, gin.H{
		"year":               y,
		"month":              m,
		"total_expense":      totalExpense,
		"total_income":       totalIncome,
		"balance":            totalIncome - totalExpense,
		"category_breakdown": categoryBreakdown,
		"count":              len(txs),
	})
}

// UpdateTransaction 更新账目
func UpdateTransaction(c *gin.Context) {
	id := c.Param("id")
	var tx models.Transaction
	if err := db.DB.First(&tx, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "账目记录不存在")
		return
	}

	var req TransactionRequest
	rawBytes, _ := io.ReadAll(c.Request.Body)
	if err := json.Unmarshal(rawBytes, &req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}
	if req.Amount == 0 || strings.TrimSpace(req.Category) == "" {
		Error(c, http.StatusBadRequest, "请填写金额、类型(支出/收入)与分类")
		return
	}
	var rawKeys map[string]json.RawMessage
	_ = json.Unmarshal(rawBytes, &rawKeys)
	oldFootprintID := uint(0)
	if tx.FootprintID != nil {
		oldFootprintID = *tx.FootprintID
	}
	if req.Amount < 0 {
		Error(c, http.StatusBadRequest, "金额必须大于 0")
		return
	}

	if tx.Source == "installment_repay" && req.Amount != tx.Amount {
		Error(c, http.StatusBadRequest, "分期还款账目金额不可直接修改；如需变动请在分期计划中撤销还款后重新标记")
		return
	}

	tx.Amount = req.Amount
	if txType := strings.TrimSpace(req.Type); txType != "" {
		if !validTxTypes[txType] {
			Error(c, http.StatusBadRequest, "type 仅支持 expense / income")
			return
		}
		tx.Type = txType
	}

	if tx.Type == "income" {
		carriesInstallmentFields := req.InstallmentChannelCode != "" || req.InstallmentTotalAmount > 0 ||
			req.InstallmentMonths > 0 || req.InstallmentMonthlyPay > 0 ||
			req.InstallmentRemainMonths > 0 || req.RepaymentDay > 0
		if (req.IsInstallment != nil && *req.IsInstallment) ||
			((tx.IsInstallment || carriesInstallmentFields) && !req.ClearInstallment) {
			Error(c, http.StatusBadRequest, "分期仅支持支出类型")
			return
		}
	}

	if req.WishID != nil {
		if *req.WishID == 0 {
			tx.WishID = nil
		} else {
			var wishCount int64
			db.DB.Unscoped().Model(&models.Wish{}).Where("id = ?", *req.WishID).Count(&wishCount)
			if wishCount == 0 {
				Error(c, http.StatusBadRequest, "关联的心愿不存在")
				return
			}
			tx.WishID = req.WishID
		}
	}
	footprintUnbound := false
	if req.FootprintID != nil {
		if *req.FootprintID == 0 {
			tx.FootprintID = nil
			footprintUnbound = true
		} else {
			var fpCount int64
			db.DB.Unscoped().Model(&models.Footprint{}).Where("id = ?", *req.FootprintID).Count(&fpCount)
			if fpCount == 0 {
				Error(c, http.StatusBadRequest, "关联的旅程不存在")
				return
			}
			tx.FootprintID = req.FootprintID
		}
	}
	wishCarriedBound := req.WishID != nil && *req.WishID > 0
	footprintCarriedBound := req.FootprintID != nil && *req.FootprintID > 0
	if (wishCarriedBound && tx.FootprintID != nil) || (footprintCarriedBound && tx.WishID != nil) {
		Error(c, http.StatusBadRequest, "心愿与足迹只能二选一")
		return
	}

	if !transactionCategoryAllowed(tx.Type, req.Category) {
		Error(c, http.StatusBadRequest, "分类不存在于分类字典")
		return
	}
	tx.Category = strings.TrimSpace(req.Category)
	if req.Title != "" || hasJSONValue(rawKeys, "title") {
		tx.Title = strings.TrimSpace(req.Title)
	}
	if req.Note != "" || hasJSONValue(rawKeys, "note") {
		tx.Note = strings.TrimSpace(req.Note)
	}
	if req.PaymentMethod != "" || hasJSONValue(rawKeys, "payment_method") {
		tx.PaymentMethod = strings.TrimSpace(req.PaymentMethod)
	}
	clearCity := req.ClearCity
	if !clearCity && req.CityCode != "" {
		cCode, _, _ := ResolveCityCodeAndName(req.CityCode, "", "")
		tx.CityCode = cCode
	}

	if req.OccurredAt != "" {
		t, err := ParseFlexTime(req.OccurredAt)
		if err != nil {
			Error(c, http.StatusBadRequest, "发生时间格式无法识别")
			return
		}
		tx.OccurredAt = t
	}

	if !footprintUnbound {
		autoLinkTransactionFootprint(&tx)
	}
	if clearCity {
		tx.CityCode = ""
	}

	if req.IsInstallment != nil && *req.IsInstallment {
		if req.InstallmentMonths > installmentMonthsLimit {
			Error(c, http.StatusBadRequest, "分期期数上限为 60 期")
			return
		}
		if !InstallmentChannelExists(nil, req.InstallmentChannelCode) {
			Error(c, http.StatusBadRequest, "分期渠道不存在")
			return
		}
		if req.InstallmentTotalAmount > 0 && req.InstallmentTotalAmount != req.Amount {
			Error(c, http.StatusBadRequest, "分期总额必须与账单金额一致")
			return
		}
		channelCode := strings.TrimSpace(req.InstallmentChannelCode)
		if channelCode == "" {
			channelCode = tx.InstallmentChannelCode
		}
		months := req.InstallmentMonths
		if months <= 0 {
			months = tx.InstallmentMonths
		}
		repayDay := req.RepaymentDay
		if repayDay <= 0 {
			repayDay = tx.RepaymentDay
		}
		AssembleInstallmentTransaction(
			&tx,
			true,
			channelCode,
			req.InstallmentTotalAmount,
			tx.Amount,
			months,
			req.InstallmentMonthlyPay,
			req.InstallmentRemainMonths,
			repayDay,
			db.DB,
		)
		if hasJSONValue(rawKeys, "installment_remain_months") && req.InstallmentRemainMonths == 0 {
			tx.InstallmentRemainMonths = 0
		}
	} else if req.ClearInstallment {
		tx.IsInstallment = false
		tx.InstallmentChannelCode = ""
		tx.InstallmentTotalAmount = 0
		tx.InstallmentMonths = 0
		tx.InstallmentMonthlyPay = 0
		tx.InstallmentRemainMonths = 0
		tx.RepaymentDay = 0
	}

	if err := db.DB.Save(&tx).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新账目失败")
		return
	}
	if tx.FootprintID != nil {
		touchFootprintTimestamps(db.DB, oldFootprintID, *tx.FootprintID)
	} else {
		touchFootprintTimestamps(db.DB, oldFootprintID)
	}
	InvalidateStatsCache()

	Success(c, tx)
}

// DeleteTransaction 删除账目 (默认软删除，?purge=true 为物理清理 BE-05)
func DeleteTransaction(c *gin.Context) {
	id := c.Param("id")
	purge := c.Query("purge") == "true"

	var tx models.Transaction
	if err := db.DB.First(&tx, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "账目不存在或已被删除")
		return
	}

	if purge {
		if err := db.DB.Transaction(func(txDB *gorm.DB) error {
			if tx.Source == "installment_purchase" && tx.InstallmentPlanID != nil {
				if err := txDB.Unscoped().Where("plan_id = ?", *tx.InstallmentPlanID).Delete(&models.InstallmentItem{}).Error; err != nil {
					return err
				}
				if err := txDB.Unscoped().Delete(&models.InstallmentPlan{}, *tx.InstallmentPlanID).Error; err != nil {
					return err
				}
			}
			if err := recordSyncTombstone(txDB, TombstoneEntityTransaction, tx.ID); err != nil {
				return err
			}
			return txDB.Unscoped().Delete(&tx).Error
		}); err != nil {
			Error(c, http.StatusInternalServerError, "物理删除失败: "+err.Error())
			return
		}
		if tx.FootprintID != nil {
			touchFootprintTimestamps(db.DB, *tx.FootprintID)
		}
		InvalidateStatsCache()
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	defer InvalidateStatsCache()

	if err := db.DB.Transaction(func(txDB *gorm.DB) error {
		if tx.Source == "installment_repay" && tx.InstallmentItemID != nil {
			if _, _, err := resetInstallmentItemOnTxDelete(txDB, *tx.InstallmentItemID); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		if tx.Source == "installment_purchase" && tx.InstallmentPlanID != nil {
			if err := txDB.Where("plan_id = ?", *tx.InstallmentPlanID).Delete(&models.InstallmentItem{}).Error; err != nil {
				return err
			}
			if err := txDB.Delete(&models.InstallmentPlan{}, *tx.InstallmentPlanID).Error; err != nil {
				return err
			}
		}

		return txDB.Delete(&tx).Error
	}); err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	if tx.FootprintID != nil {
		touchFootprintTimestamps(db.DB, *tx.FootprintID)
	}
	Success(c, gin.H{"id": id, "deleted": true})
}
