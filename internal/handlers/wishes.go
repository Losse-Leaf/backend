package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

// populateWishURLs 将心愿参考图签名转化为可访问链接
func populateWishURLs(w *models.Wish) {
	for i := range w.Attachments {
		w.Attachments[i].URL = storage.SignResourceURL(w.Attachments[i].ObjectKey)
	}
}

// CreateWishRequest 新建心愿请求
type CreateWishRequest struct {
	Title         string `json:"title" binding:"required"`
	Kind          string `json:"kind" binding:"required"` // item | travel | milestone
	Note          string `json:"note"`
	Spec          string `json:"spec"`
	CityCode      string `json:"city_code"`
	CityName      string `json:"city_name"`
	Budget        int64  `json:"budget"`
	TargetDate    string `json:"target_date"`
	AttachmentIDs []uint `json:"attachment_ids"`
}

// UpdateWishRequest 更新心愿请求
type UpdateWishRequest struct {
	Title         string `json:"title"`
	Kind          string `json:"kind"`
	Note          string `json:"note"`
	Spec          string `json:"spec"`
	CityCode      string `json:"city_code"`
	CityName      string `json:"city_name"`
	Budget        int64  `json:"budget"`
	Status        string `json:"status"` // wanting | fulfilled | shelved
	TargetDate    string `json:"target_date"`
	AttachmentIDs []uint `json:"attachment_ids"`
}

// ResolveWishRequest 归册仪式请求 (4种归册情景与多模块联动 RC-53)
type ResolveWishRequest struct {
	FulfilledAt            string `json:"fulfilled_at"` // 默认当前，支持历史补录，强制 <= now
	ActualCost             int64  `json:"actual_cost"`  // 单位：分
	ResolutionType         string `json:"resolution_type"` // exact(原配原价) | altered(改选替代) | relinquished(理性拔草)
	ActualTitle            string `json:"actual_title"` // 实际购买物品名 (altered 专用)
	CreateTransaction      bool   `json:"create_transaction"`
	CreateGearAsset        bool   `json:"create_gear_asset"` // item 专属一键建入随身设备库
	CreateLifeEntry        bool   `json:"create_life_entry"`
	CreateFootprint        bool   `json:"create_footprint"`
	EntryContent           string `json:"entry_content"`
	AttachmentIDs          []uint `json:"attachment_ids"`
	// 分期付款支持 (RC-53)
	IsInstallment          bool   `json:"is_installment"`
	InstallmentChannelCode string `json:"installment_channel_code"`
	InstallmentMonths      int    `json:"installment_months"`
	RepaymentDay           int    `json:"repayment_day"`
}

// GetWishes 列表查询心愿 (支持 kind, status, since, 分页与 ETag)
func GetWishes(c *gin.Context) {
	kind := c.Query("kind")
	status := c.Query("status")
	sinceStr := c.Query("since")
	includeDeleted := c.Query("include_deleted") == "true" || sinceStr != ""

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}

	dbScope := db.DB
	if includeDeleted {
		dbScope = db.DB.Unscoped()
	}

	query := dbScope.Model(&models.Wish{}).Preload("Attachments").Order("created_at DESC, id DESC")

	if sinceStr != "" {
		if sinceTime, err := ParseFlexTime(sinceStr); err == nil {
			query = query.Where("updated_at > ? OR deleted_at > ?", sinceTime, sinceTime)
		}
	}

	if kind != "" && kind != "all" {
		query = query.Where("kind = ?", kind)
	}
	if status != "" && status != "all" {
		query = query.Where("status = ?", status)
	}

	var total int64
	query.Count(&total)

	var wishes []models.Wish
	offset := (page - 1) * pageSize
	if err := query.Offset(offset).Limit(pageSize).Find(&wishes).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取心愿列表失败")
		return
	}

	for i := range wishes {
		populateWishURLs(&wishes[i])
	}

	// ETag 与 304 缓存协商
	var maxUpdated time.Time
	for _, w := range wishes {
		if w.UpdatedAt.After(maxUpdated) {
			maxUpdated = w.UpdatedAt
		}
		if w.DeletedAt.Valid && w.DeletedAt.Time.After(maxUpdated) {
			maxUpdated = w.DeletedAt.Time
		}
	}
	etag := fmt.Sprintf(`W/"%d-%d"`, total, maxUpdated.UnixMilli())
	c.Header("ETag", etag)

	if match := c.GetHeader("If-None-Match"); match != "" && match == etag {
		c.Status(http.StatusNotModified)
		return
	}

	Success(c, gin.H{
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"wishes":    wishes,
	})
}

// GetWishByID 单个心愿详情
func GetWishByID(c *gin.Context) {
	id := c.Param("id")
	var w models.Wish
	if err := db.DB.Preload("Attachments").First(&w, id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}
	populateWishURLs(&w)
	Success(c, w)
}

// CreateWish 新建心愿 (需管理员权限)
func CreateWish(c *gin.Context) {
	var req CreateWishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "心愿标题与类型必填")
		return
	}

	var targetDate *time.Time
	if req.TargetDate != "" {
		if t, err := ParseFlexTime(req.TargetDate); err == nil {
			targetDate = &t
		}
	}

	wish := models.Wish{
		Title:      strings.TrimSpace(req.Title),
		Kind:       strings.TrimSpace(req.Kind),
		Note:       strings.TrimSpace(req.Note),
		Spec:       strings.TrimSpace(req.Spec),
		CityCode:   strings.TrimSpace(req.CityCode),
		CityName:   strings.TrimSpace(req.CityName),
		Budget:     req.Budget,
		Status:     "wanting",
		TargetDate: targetDate,
	}

	if err := db.DB.Create(&wish).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建心愿失败: "+err.Error())
		return
	}

	if len(req.AttachmentIDs) > 0 {
		db.DB.Model(&models.Attachment{}).
			Where("id IN ?", req.AttachmentIDs).
			Update("wish_id", wish.ID)
	}

	db.DB.Preload("Attachments").First(&wish, wish.ID)
	populateWishURLs(&wish)

	Success(c, wish)
}

// UpdateWish 更新心愿
func UpdateWish(c *gin.Context) {
	id := c.Param("id")
	var wish models.Wish
	if err := db.DB.Preload("Attachments").First(&wish, id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}

	var req UpdateWishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	if req.Title != "" {
		wish.Title = strings.TrimSpace(req.Title)
	}
	if req.Kind != "" {
		wish.Kind = strings.TrimSpace(req.Kind)
	}
	if req.Note != "" {
		wish.Note = strings.TrimSpace(req.Note)
	}
	if req.Spec != "" {
		wish.Spec = strings.TrimSpace(req.Spec)
	}
	if req.CityCode != "" {
		wish.CityCode = strings.TrimSpace(req.CityCode)
	}
	if req.CityName != "" {
		wish.CityName = strings.TrimSpace(req.CityName)
	}
	if req.Budget != 0 {
		wish.Budget = req.Budget
	}
	if req.Status != "" {
		wish.Status = strings.TrimSpace(req.Status)
	}
	if req.TargetDate != "" {
		if t, err := ParseFlexTime(req.TargetDate); err == nil {
			wish.TargetDate = &t
		}
	}

	if err := db.DB.Save(&wish).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新心愿失败")
		return
	}

	if req.AttachmentIDs != nil {
		db.DB.Model(&models.Attachment{}).
			Where("wish_id = ? AND id NOT IN ?", wish.ID, req.AttachmentIDs).
			Update("wish_id", nil)

		if len(req.AttachmentIDs) > 0 {
			db.DB.Model(&models.Attachment{}).
				Where("id IN ?", req.AttachmentIDs).
				Update("wish_id", wish.ID)
		}
	}

	db.DB.Preload("Attachments").First(&wish, wish.ID)
	populateWishURLs(&wish)

	Success(c, wish)
}

// ResolveWish 核心归册仪式事务 (方案 B 多模块自动联动)
func ResolveWish(c *gin.Context) {
	id := c.Param("id")
	var wish models.Wish
	if err := db.DB.Preload("Attachments").First(&wish, id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}

	var req ResolveWishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求格式无效")
		return
	}

	// 达成时间解析与校验 (必须 <= now，杜绝穿越)
	fulfilledAt := time.Now()
	if req.FulfilledAt != "" {
		if t, err := ParseFlexTime(req.FulfilledAt); err == nil {
			fulfilledAt = t
		}
	}
	if fulfilledAt.After(time.Now()) {
		Error(c, http.StatusBadRequest, "达成时间不能晚于当前时间")
		return
	}

	resType := strings.TrimSpace(req.ResolutionType)
	if resType == "" {
		resType = "exact"
	}
	if resType != "exact" && resType != "altered" && resType != "relinquished" {
		resType = "exact"
	}

	actualTitle := strings.TrimSpace(req.ActualTitle)
	if actualTitle == "" {
		actualTitle = wish.Title
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		wish.ResolutionType = resType
		wish.FulfilledAt = &fulfilledAt

		// 情景 3: 理性拔草 / 释怀放弃 (Relinquished with Peace RC-53)
		if resType == "relinquished" {
			wish.Status = "relinquished"
			wish.ActualCost = 0
			// 不生成支出流水，不增加设备库，不产生任何负债
			if req.CreateLifeEntry {
				content := strings.TrimSpace(req.EntryContent)
				if content == "" {
					content = fmt.Sprintf("理性拔草 · 释怀放下心愿「%s」", wish.Title)
				}
				entry := models.LifeEntry{
					Type:       "reflection",
					Title:      "理性拔草 · " + wish.Title,
					Content:    content,
					OccurredAt: fulfilledAt,
					Tags:       "#心愿释怀",
					CityCode:   wish.CityCode,
					CityName:   wish.CityName,
				}
				if err := tx.Create(&entry).Error; err != nil {
					return err
				}
				wish.LifeEntryID = &entry.ID
				if len(req.AttachmentIDs) > 0 {
					tx.Model(&models.Attachment{}).
						Where("id IN ?", req.AttachmentIDs).
						Update("entry_id", entry.ID)
				}
			}
			return tx.Save(&wish).Error
		}

		// 情景 1 & 2: 原配原价 (exact) 或 改选替代方案 (altered)
		wish.Status = "fulfilled"
		wish.ActualCost = req.ActualCost
		if resType == "altered" {
			wish.ActualTitle = actualTitle
		}

		// 联动 1: 账本支出流水 (支持分期信贷联动 RC-53)
		if req.CreateTransaction && req.ActualCost > 0 {
			category := "好物购置"
			if wish.Kind == "travel" {
				category = "旅行度假"
			}
			displayTitle := "心愿达成: " + wish.Title
			if resType == "altered" && actualTitle != "" {
				displayTitle = fmt.Sprintf("心愿替代达成: %s (原: %s)", actualTitle, wish.Title)
			}
			newTx := models.Transaction{
				Amount:        req.ActualCost,
				Type:          "expense",
				Category:      category,
				Title:         displayTitle,
				Note:          fmt.Sprintf("三维心愿归册支出 (类别: %s, 方案: %s)", wish.Kind, resType),
				PaymentMethod: "心愿支出",
				WishID:        &wish.ID,
				CityCode:      wish.CityCode,
				OccurredAt:    fulfilledAt,
			}

			if req.IsInstallment {
				newTx.IsInstallment = true
				newTx.InstallmentChannelCode = strings.TrimSpace(req.InstallmentChannelCode)
				if newTx.InstallmentChannelCode == "" {
					newTx.InstallmentChannelCode = "huabei"
				}
				newTx.InstallmentTotalAmount = req.ActualCost
				months := req.InstallmentMonths
				if months <= 0 {
					months = 12
				}
				newTx.InstallmentMonths = months
				newTx.InstallmentMonthlyPay = req.ActualCost / int64(months)
				newTx.InstallmentRemainMonths = months

				rDay := req.RepaymentDay
				if rDay <= 0 {
					var ch models.CreditChannel
					if err := tx.Where("code = ?", newTx.InstallmentChannelCode).First(&ch).Error; err == nil {
						rDay = ch.RepaymentDay
					} else {
						rDay = 10
					}
				}
				newTx.RepaymentDay = rDay
			}

			if err := tx.Create(&newTx).Error; err != nil {
				return err
			}
			wish.TransactionID = &newTx.ID
		}

		// 联动 2: 固定资产设备入库 (item 专属，双向联动 RC-53)
		if req.CreateGearAsset && wish.Kind == "item" {
			gearName := actualTitle
			if gearName == "" {
				gearName = wish.Title
			}
			gearID := fmt.Sprintf("gear_%d", time.Now().UnixNano())
			wish.GearID = &gearID

			var profile models.CuratorProfile
			if err := tx.First(&profile).Error; err == nil {
				newGear := models.GearItem{
					ID:           gearID,
					Name:         gearName,
					Spec:         wish.Spec,
					Category:     "数码器材",
					Status:       "active",
					PriceCents:   req.ActualCost,
					PurchaseDate: fulfilledAt.Format("2006-01-02"),
					Note:         fmt.Sprintf("心愿归册入库 (心愿 #%d: %s)", wish.ID, wish.Title),
				}
				list := loadProfileGear(&profile)
				list = append(list, newGear)
				bytes, _ := json.Marshal(list)
				profile.EDCGear = string(bytes)
				profile.UpdatedAt = time.Now()
				_ = tx.Save(&profile).Error
			}
		}

		// 联动 3: 时间流散页随笔
		if req.CreateLifeEntry {
			content := strings.TrimSpace(req.EntryContent)
			if content == "" {
				content = fmt.Sprintf("圆满达成心愿「%s」", actualTitle)
			}
			entryType := "collection"
			if wish.Kind == "travel" {
				entryType = "life"
			}
			entry := models.LifeEntry{
				Type:       entryType,
				Title:      "心愿达成 · " + actualTitle,
				Content:    content,
				OccurredAt: fulfilledAt,
				Tags:       "#心愿达成",
				CityCode:   wish.CityCode,
				CityName:   wish.CityName,
			}
			if err := tx.Create(&entry).Error; err != nil {
				return err
			}
			wish.LifeEntryID = &entry.ID

			if len(req.AttachmentIDs) > 0 {
				tx.Model(&models.Attachment{}).
					Where("id IN ?", req.AttachmentIDs).
					Update("entry_id", entry.ID)
			}
		}

		// 联动 4: 足迹打卡 (travel 专属)
		if req.CreateFootprint && wish.Kind == "travel" && wish.CityCode != "" {
			cityName := wish.CityName
			if cityName == "" {
				cityName = "旅行目的地"
			}
			footprint := models.Footprint{
				CityCode:    wish.CityCode,
				CityName:    cityName,
				VisitedAt:   fulfilledAt,
				Note:        "心愿达成履迹: " + wish.Title,
				WishID:      &wish.ID,
				LifeEntryID: wish.LifeEntryID,
			}
			if err := tx.Create(&footprint).Error; err != nil {
				return err
			}
			wish.FootprintID = &footprint.ID

			if len(req.AttachmentIDs) > 0 {
				tx.Model(&models.Attachment{}).
					Where("id IN ?", req.AttachmentIDs).
					Update("footprint_id", footprint.ID)
			}
		}

		// 保存心愿本身
		return tx.Save(&wish).Error
	})

	if err != nil {
		Error(c, http.StatusInternalServerError, "心愿归册事务执行失败: "+err.Error())
		return
	}

	db.DB.Preload("Attachments").First(&wish, wish.ID)
	populateWishURLs(&wish)

	Success(c, wish)
}

// DeleteWish 删除心愿 (默认软删除，?purge=true 为物理清理)
func DeleteWish(c *gin.Context) {
	id := c.Param("id")
	purge := c.Query("purge") == "true"

	var w models.Wish
	if err := db.DB.First(&w, id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在或已被删除")
		return
	}

	if purge {
		if err := db.DB.Unscoped().Delete(&w).Error; err != nil {
			Error(c, http.StatusInternalServerError, "物理删除失败: "+err.Error())
			return
		}
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	if err := db.DB.Delete(&w).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	Success(c, gin.H{"id": id, "deleted": true})
}
