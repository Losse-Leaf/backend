package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

// populateWishURLs 通过纯逻辑外键加载心愿参考图并签名转化为可访问链接
func populateWishURLs(w *models.Wish) {
	if w == nil {
		return
	}
	if w.ID > 0 {
		var atts []models.Attachment
		db.DB.Where("wish_id = ?", w.ID).Order("id ASC").Find(&atts)
		w.Attachments = atts
	}
	if w.Attachments == nil {
		w.Attachments = []models.Attachment{}
	}
	for i := range w.Attachments {
		w.Attachments[i].URL = storage.SignResourceURL(w.Attachments[i].ObjectKey)
		w.Attachments[i].ThumbnailURL = ThumbnailURLFor(w.Attachments[i].ObjectKey, 0)
	}
}

// populateWishURLsBatch 批量填充一组心愿的参考图 (BE-30)
func populateWishURLsBatch(wishes []models.Wish) {
	if len(wishes) == 0 {
		return
	}
	ids := make([]uint, 0, len(wishes))
	for i := range wishes {
		if wishes[i].ID > 0 {
			ids = append(ids, wishes[i].ID)
		}
	}
	attMap := make(map[uint][]models.Attachment)
	if len(ids) > 0 {
		var atts []models.Attachment
		db.DB.Where("wish_id IN ?", ids).Order("id ASC").Find(&atts)
		for _, att := range atts {
			if att.WishID != nil {
				attMap[*att.WishID] = append(attMap[*att.WishID], att)
			}
		}
	}
	for i := range wishes {
		atts := attMap[wishes[i].ID]
		if atts == nil {
			atts = []models.Attachment{}
		}
		for j := range atts {
			atts[j].URL = storage.SignResourceURL(atts[j].ObjectKey)
			atts[j].ThumbnailURL = ThumbnailURLFor(atts[j].ObjectKey, 0)
		}
		wishes[i].Attachments = atts
	}
}

// CreateWishRequest 新建心愿请求
type CreateWishRequest struct {
	Title string `json:"title" binding:"required"`
	// MOB-P1-35：省略即默认 item
	Kind       string `json:"kind"` // item | travel | milestone
	Note       string `json:"note"`
	Spec       string `json:"spec"`
	CityCode   string `json:"city_code"`
	CityName   string `json:"city_name"`
	Budget     int64  `json:"budget"`
	TargetDate string `json:"target_date"`
	// PlantedAt 许下心愿的时刻（可选，支持补录过去的心愿）
	PlantedAt     string `json:"planted_at"`
	AttachmentIDs []uint `json:"attachment_ids"`
}

// UpdateWishRequest 更新心愿请求
type UpdateWishRequest struct {
	Title string `json:"title"`
	Kind  string `json:"kind"`
	// PlantedAt 许下心愿的时刻（可选，支持补档）
	PlantedAt     string `json:"planted_at"`
	Note          string `json:"note"`
	Spec          string `json:"spec"`
	CityCode      string `json:"city_code"`
	CityName      string `json:"city_name"`
	Budget        int64  `json:"budget"`
	ActualCost    *int64 `json:"actual_cost"`
	Status        string `json:"status"` // wanting | fulfilled | shelved
	TargetDate    string `json:"target_date"`
	FulfilledAt   string `json:"fulfilled_at"`
	AttachmentIDs []uint `json:"attachment_ids"`
}

// ResolveWishRequest 归册仪式请求 (4种归册情景与多模块联动 RC-53)
type ResolveWishRequest struct {
	FulfilledAt       string `json:"fulfilled_at"`    // 默认当前，支持历史补录，强制 <= now
	ActualCost        int64  `json:"actual_cost"`     // 单位：分
	ResolutionType    string `json:"resolution_type"` // exact(原配原价) | altered(改选替代) | relinquished(理性拔草)
	ActualTitle       string `json:"actual_title"`    // 实际购买物品名 (altered 专用)
	CreateTransaction bool   `json:"create_transaction"`
	CreateGearAsset   bool   `json:"create_gear_asset"` // item 专属一键建入随身设备库
	CreateLifeEntry   bool   `json:"create_life_entry"`
	CreateFootprint   bool   `json:"create_footprint"`
	EntryContent      string `json:"entry_content"`
	AttachmentIDs     []uint `json:"attachment_ids"`
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

	query := dbScope.Model(&models.Wish{}).Order("created_at DESC, id DESC")

	// MOB-P1-36：增量游标过滤统一走 applySyncSince（边界含等号）
	query = applySyncSince(query, sinceStr)

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

	isAdmin := middleware.IsAdmin(c)
	populateWishURLsBatch(wishes)
	for i := range wishes {
		// 访客模式财务隐私严格脱敏 (BE-14)
		if !isAdmin {
			wishes[i].ActualCost = 0
			wishes[i].TransactionID = nil
			wishes[i].GearID = nil
		}
	}

	// ETag 与 304 缓存协商 (附带权限角色后缀防止跨权限 304 缓存污染 BE-17)
	var maxUpdated time.Time
	for _, w := range wishes {
		if w.UpdatedAt.After(maxUpdated) {
			maxUpdated = w.UpdatedAt
		}
		if w.DeletedAt.Valid && w.DeletedAt.Time.After(maxUpdated) {
			maxUpdated = w.DeletedAt.Time
		}
	}
	role := "guest"
	if isAdmin {
		role = "admin"
	}
	etag := fmt.Sprintf(`W/"%d-%d-%s"`, total, maxUpdated.UnixMilli(), role)
	c.Header("ETag", etag)

	// MOB-P1-36：服务端权威增量游标
	setSyncCursor(c, maxUpdated)
	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")

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

// GetWishByID 单个心愿详情 (访客模式脱敏 BE-14)
func GetWishByID(c *gin.Context) {
	id := c.Param("id")
	var w models.Wish
	if err := db.DB.First(&w, id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}
	populateWishURLs(&w)
	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	if !middleware.IsAdmin(c) {
		w.ActualCost = 0
		w.TransactionID = nil
		w.GearID = nil
	}
	Success(c, w)
}

// CreateWish 新建心愿 (需管理员权限，自动伴生创建一条 wish 类型的一级记录写入主时间流)
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

	// 补档：许下时刻可指定为过去（省略则取当前）
	var plantedAt *time.Time
	if req.PlantedAt != "" {
		if t, err := ParseFlexTime(req.PlantedAt); err == nil {
			plantedAt = &t
		}
	}
	plantedMoment := time.Now().In(config.BeijingLocation)
	if plantedAt != nil {
		plantedMoment = *plantedAt
	}

	// MOB-P1-35：省略 kind 时回落到默认 item
	wishKind := strings.TrimSpace(req.Kind)
	if wishKind == "" {
		wishKind = "item"
	}

	wish := models.Wish{
		Title:      strings.TrimSpace(req.Title),
		Kind:       wishKind,
		Note:       strings.TrimSpace(req.Note),
		Spec:       strings.TrimSpace(req.Spec),
		CityCode:   strings.TrimSpace(req.CityCode),
		CityName:   strings.TrimSpace(req.CityName),
		Budget:     req.Budget,
		Status:     "wanting",
		TargetDate: targetDate,
		PlantedAt:  plantedAt,
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&wish).Error; err != nil {
			return err
		}

		entryContent := wish.Note
		if entryContent == "" {
			if wish.Spec != "" {
				entryContent = fmt.Sprintf("许下心愿「%s」（%s）。", wish.Title, wish.Spec)
			} else {
				entryContent = fmt.Sprintf("许下心愿「%s」，期待日后圆满归册。", wish.Title)
			}
		}

		companionEntry := models.LifeEntry{
			Type:    "wish",
			Title:   "许下心愿 · " + wish.Title,
			Content: entryContent,
			// 关键：伴生散页落在**实际许下的时刻**，补档的心愿才会出现在时间流的正确位置
			OccurredAt: plantedMoment,
			Location:   wish.CityName,
			CityCode:   wish.CityCode,
			CityName:   wish.CityName,
			WishID:     &wish.ID,
		}
		if err := tx.Create(&companionEntry).Error; err != nil {
			return err
		}

		wish.LifeEntryID = &companionEntry.ID
		if err := tx.Save(&wish).Error; err != nil {
			return err
		}

		if len(req.AttachmentIDs) > 0 {
			if err := tx.Model(&models.Attachment{}).
				Where("id IN ?", req.AttachmentIDs).
				Updates(map[string]interface{}{
					"wish_id":  wish.ID,
					"entry_id": companionEntry.ID,
				}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "创建心愿失败: "+err.Error())
		return
	}

	db.DB.First(&wish, wish.ID)
	populateWishURLs(&wish)
	InvalidateStatsCache()

	Success(c, wish)
}

// UpdateWish 更新心愿
func UpdateWish(c *gin.Context) {
	id := c.Param("id")
	var wish models.Wish
	if err := db.DB.First(&wish, id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}

	var req UpdateWishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	oldStatus := wish.Status
	oldTitle := wish.Title

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
	if req.ActualCost != nil {
		wish.ActualCost = *req.ActualCost
	}
	if req.Status != "" {
		wish.Status = strings.TrimSpace(req.Status)
	}
	if req.TargetDate != "" {
		if t, err := ParseFlexTime(req.TargetDate); err == nil {
			wish.TargetDate = &t
		}
	}
	// 补档：许下时刻可改写为过去的时间
	if req.PlantedAt != "" {
		if t, err := ParseFlexTime(req.PlantedAt); err == nil {
			wish.PlantedAt = &t
		}
	}
	if req.FulfilledAt != "" {
		if t, err := ParseFlexTime(req.FulfilledAt); err == nil {
			wish.FulfilledAt = &t
		}
	}
	if wish.Status == "fulfilled" && wish.FulfilledAt == nil {
		now := time.Now()
		wish.FulfilledAt = &now
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		// 若通过直接更新状态标记心愿达成，自动伴生创建一条达成一级记录 (Record-First Architecture，纳入同一事务)
		if wish.Status == "fulfilled" && oldStatus != "fulfilled" {
			entryType := "wish"
			if wish.Kind == "travel" && wish.CityCode != "" {
				entryType = "travel"
			}
			fulfilledTime := time.Now()
			if wish.FulfilledAt != nil {
				fulfilledTime = *wish.FulfilledAt
			}
			entryContent := strings.TrimSpace(wish.Note)
			if entryContent == "" {
				entryContent = fmt.Sprintf("圆满达成心愿「%s」", wish.Title)
			}
			companionEntry := models.LifeEntry{
				Type:       entryType,
				Title:      "心愿达成 · " + wish.Title,
				Content:    entryContent,
				OccurredAt: fulfilledTime,
				Location:   wish.CityName,
				CityCode:   wish.CityCode,
				CityName:   wish.CityName,
				WishID:     &wish.ID,
			}
			if err := tx.Create(&companionEntry).Error; err != nil {
				return err
			}
			wish.LifeEntryID = &companionEntry.ID
			syncFootprintFromLifeEntry(&companionEntry, req.AttachmentIDs, tx)
		}

		// 普通编辑（未触发达成）时，同步更新「许下心愿」那条伴生散页，
		// 避免时间流里留着旧标题 / 旧内容。
		if !(wish.Status == "fulfilled" && oldStatus != "fulfilled") {
			if err := syncWishCompanionEntry(tx, wish, oldTitle); err != nil {
				return err
			}
			// 补档：许下时刻变了，伴生散页的 occurred_at 要跟着走，
			// 否则补录的心愿仍然会落在今天。
			if wish.PlantedAt != nil {
				if err := tx.Model(&models.LifeEntry{}).
					Where("wish_id = ? AND title = ?", wish.ID, companionWishPlantedTitlePrefix+wish.Title).
					Update("occurred_at", *wish.PlantedAt).Error; err != nil {
					return err
				}
			}
		}

		if err := tx.Save(&wish).Error; err != nil {
			return err
		}

		if req.AttachmentIDs != nil {
			// 空数组语义 = 解除该心愿的全部参考图绑定
			if len(req.AttachmentIDs) == 0 {
				tx.Model(&models.Attachment{}).
					Where("wish_id = ?", wish.ID).
					Update("wish_id", nil)
			} else {
				tx.Model(&models.Attachment{}).
					Where("wish_id = ? AND id NOT IN ?", wish.ID, req.AttachmentIDs).
					Update("wish_id", nil)

				updates := map[string]interface{}{
					"wish_id": wish.ID,
				}
				if wish.LifeEntryID != nil && *wish.LifeEntryID > 0 {
					updates["entry_id"] = *wish.LifeEntryID
				}
				tx.Model(&models.Attachment{}).
					Where("id IN ?", req.AttachmentIDs).
					Updates(updates)
			}
		}
		return nil
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "更新心愿事务执行失败: "+err.Error())
		return
	}

	db.DB.First(&wish, wish.ID)
	populateWishURLs(&wish)
	InvalidateStatsCache()

	Success(c, wish)
}

// ResolveWish 核心归册仪式事务 (方案 B 多模块自动联动)
func ResolveWish(c *gin.Context) {
	id := c.Param("id")
	var wish models.Wish
	if err := db.DB.First(&wish, id).Error; err != nil {
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
					Type:       "wish",
					Title:      "理性拔草 · " + wish.Title,
					Content:    content,
					OccurredAt: fulfilledAt,
					Location:   wish.CityName,
					WishID:     &wish.ID,
				}
				if err := tx.Create(&entry).Error; err != nil {
					return err
				}
				wish.LifeEntryID = &entry.ID
				if len(req.AttachmentIDs) > 0 {
					tx.Model(&models.Attachment{}).
						Where("id IN ?", req.AttachmentIDs).
						Updates(map[string]interface{}{
							"wish_id":  wish.ID,
							"entry_id": entry.ID,
						})
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
				AssembleInstallmentTransaction(
					&newTx,
					req.IsInstallment,
					req.InstallmentChannelCode,
					req.ActualCost,
					req.ActualCost,
					req.InstallmentMonths,
					0,
					req.InstallmentMonths,
					req.RepaymentDay,
					tx,
				)
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
			gearID := fmt.Sprintf("gear_%s", generateRandomHex(8))
			wish.GearID = &gearID

			newGear := models.GearItem{
				ID:           gearID,
				Name:         gearName,
				Spec:         wish.Spec,
				Category:     "数码器材",
				Status:       "active",
				PriceCents:   req.ActualCost,
				PurchaseDate: fulfilledAt.In(config.BeijingLocation).Format("2006-01-02"),
				Note:         fmt.Sprintf("心愿归册入库 (心愿 #%d: %s)", wish.ID, wish.Title),
			}

			// EDCGear 同行 JSON 列读-改-写须串行化 (BE-34)，写入仍在当前事务内保持原子
			profileGearMu.Lock()
			defer profileGearMu.Unlock()
			var profile models.CuratorProfile
			if err := tx.First(&profile).Error; err == nil {
				list := append(loadProfileGear(&profile), newGear)
				if err := saveProfileGearWith(tx, &profile, list); err != nil {
					return err
				}
			}
		}

		// 联动 3: 时间流散页随笔 (Record-First: 若勾选了生成足迹或生成动态，均保证伴生一级记录)
		if req.CreateLifeEntry || (req.CreateFootprint && wish.Kind == "travel" && wish.CityCode != "") {
			content := strings.TrimSpace(req.EntryContent)
			if content == "" {
				content = fmt.Sprintf("圆满达成心愿「%s」", actualTitle)
			}
			entryType := "wish"
			if wish.Kind == "travel" && req.CreateFootprint {
				entryType = "travel"
			}
			entry := models.LifeEntry{
				Type:       entryType,
				Title:      "心愿达成 · " + actualTitle,
				Content:    content,
				OccurredAt: fulfilledAt,
				Location:   wish.CityName,
				CityCode:   wish.CityCode,
				CityName:   wish.CityName,
				WishID:     &wish.ID,
			}
			if err := tx.Create(&entry).Error; err != nil {
				return err
			}
			wish.LifeEntryID = &entry.ID

			if len(req.AttachmentIDs) > 0 {
				tx.Model(&models.Attachment{}).
					Where("id IN ?", req.AttachmentIDs).
					Updates(map[string]interface{}{
						"wish_id":  wish.ID,
						"entry_id": entry.ID,
					})
			}
		}

		// 联动 4: 足迹打卡 (travel 专属，仅当用户明确勾选 CreateFootprint 时生成)
		if req.CreateFootprint && wish.Kind == "travel" && wish.CityCode != "" {
			cityName := wish.CityName
			if cityName == "" {
				cityName = "旅行目的地"
			}
			fpNote := strings.TrimSpace(req.EntryContent)
			if fpNote == "" {
				fpNote = "心愿达成履迹: " + wish.Title
			}
			footprint := models.Footprint{
				CityCode:    wish.CityCode,
				CityName:    cityName,
				VisitedAt:   fulfilledAt,
				Note:        fpNote,
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

	db.DB.First(&wish, wish.ID)
	populateWishURLs(&wish)
	InvalidateStatsCache()

	Success(c, wish)
}

// DeleteWish 删除心愿 (默认软删除，?purge=true 为物理清理；解绑关联流水与动态逻辑外键)
func DeleteWish(c *gin.Context) {
	id := c.Param("id")
	purge := c.Query("purge") == "true"

	var w models.Wish
	if err := db.DB.First(&w, id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在或已被删除")
		return
	}

	// 关键：删除心愿时**一并软删除**它在时间流里的伴生散页（两条删除路径都要走）。
	// 原先只是把 wish_id 置空，导致时间流里留下一条再也点不回心愿的孤儿记录。
	if err := db.DB.Where("wish_id = ?", w.ID).Delete(&models.LifeEntry{}).Error; err != nil {
		Error(c, http.StatusInternalServerError, "清理伴生记录失败: "+err.Error())
		return
	}

	if purge {
		db.DB.Model(&models.Transaction{}).Where("wish_id = ?", w.ID).Update("wish_id", nil)
		db.DB.Model(&models.Footprint{}).Where("wish_id = ?", w.ID).Update("wish_id", nil)
		db.DB.Model(&models.Attachment{}).Where("wish_id = ?", w.ID).Update("wish_id", nil)
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
