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

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

// validWishKinds / validWishStatuses 心愿类型与状态白名单：
// 非法值直接 400 —— kind/status 都是列表筛选与归册联动的分支维度，
// 落库一个未知值会让心愿从对应视角里静默消失。
var validWishKinds = map[string]bool{
	"item":      true,
	"travel":    true,
	"milestone": true,
}

var validWishStatuses = map[string]bool{
	"wanting":      true,
	"fulfilled":    true,
	"shelved":      true,
	"relinquished": true,
}

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
	w.Attachments = populateAttachmentURLs(w.Attachments)
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
		wishes[i].Attachments = populateAttachmentURLs(attMap[wishes[i].ID])
	}
}

// CreateWishRequest 新建心愿请求
type CreateWishRequest struct {
	Title string `json:"title" binding:"required"`
	// MOB-P1-35：省略即默认 item
	Kind       string `json:"kind"` // item | travel | milestone
	Note       string `json:"note"`
	Spec       string `json:"spec"`
	Budget     int64  `json:"budget"`
	TargetDate string `json:"target_date"`
	// PlantedAt 许下心愿的时刻（可选，支持补录过去的心愿）
	PlantedAt     string `json:"planted_at"`
	AttachmentIDs []uint `json:"attachment_ids"`
	// ClientRequestID 幂等创建标识（跨端统一契约 B）：
	// 客户端为每次创建生成稳定 UUID；同一 id 的重复提交直接返回已有记录（HTTP 200）。
	ClientRequestID string `json:"client_request_id"`
	IsPrivate       bool   `json:"is_private"`
}

// UpdateWishRequest 更新心愿请求
type UpdateWishRequest struct {
	Title string `json:"title"`
	Kind  string `json:"kind"`
	// PlantedAt 许下心愿的时刻（可选，支持补档）
	PlantedAt        string `json:"planted_at"`
	Note             string `json:"note"`
	Spec             string `json:"spec"`
	Budget           int64  `json:"budget"`
	ActualCost       *int64 `json:"actual_cost"`
	Status           string `json:"status"` // wanting | fulfilled | shelved
	TargetDate       string `json:"target_date"`
	FulfilledAt      string `json:"fulfilled_at"`
	AttachmentIDs    []uint `json:"attachment_ids"`
	ClearAttachments bool   `json:"clear_attachments"`
	IsPrivate        *bool  `json:"is_private"`
}

// ResolveWishRequest 归册仪式请求 (心愿转记账流水与伴生散页联动)
type ResolveWishRequest struct {
	FulfilledAt       string `json:"fulfilled_at"`    // 默认当前，支持历史补录，强制 <= now
	ActualCost        int64  `json:"actual_cost"`     // 单位：分
	ResolutionType    string `json:"resolution_type"` // exact(原配原价) | altered(改选替代) | relinquished(理性拔草)
	ActualTitle       string `json:"actual_title"`    // 实际购买物品名 (altered 专用)
	CreateTransaction bool   `json:"create_transaction"`
	CreateLifeEntry   bool   `json:"create_life_entry"`
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
	if !RequirePublicModuleOrAdmin(c, "wishes") {
		return
	}
	kind := c.Query("kind")
	status := c.Query("status")
	sinceStr := c.Query("since")
	includeDeleted := c.Query("include_deleted") == "true" || sinceStr != ""
	if !validateSyncSince(c, sinceStr) || !allowDeletedScope(c, includeDeleted) {
		return
	}

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
	query = applySyncSince(query, &models.Wish{}, sinceStr)

	if kind != "" && kind != "all" {
		query = query.Where("kind = ?", kind)
	}
	if status != "" && status != "all" {
		query = query.Where("status = ?", status)
	}

	isAdmin := middleware.IsAdmin(c)
	if !isAdmin {
		query = query.Where("is_private = ?", false)
	}
	role := PublicModuleETagSuffix(isAdmin)
	// Vary 必须在任何 304 分支之前写出，避免共享缓存把访客/管理员响应混用。
	setAuthVaryHeader(c)

	// BE-2：聚合与 304 协商前置；ETag 混入角色，防止跨权限缓存污染 (BE-17)。
	agg, err := aggregateListQuery(query)
	if err != nil {
		Error(c, http.StatusInternalServerError, "获取心愿列表失败")
		return
	}
	// 增量请求（since）跳过 304：purge 墓碑不在聚合 SQL 里，命中 304 会漏删。
	if sinceStr == "" && negotiateListCache(c, listETag(c, agg, role)) {
		return
	}

	var wishes []models.Wish
	offset := (page - 1) * pageSize
	if err := query.Offset(offset).Limit(pageSize).Find(&wishes).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取心愿列表失败")
		return
	}

	// P2-3：只在增量第一页注入 purge 墓碑。
	if page == 1 && sinceStr != "" {
		if err := injectTombstones(&wishes, TombstoneEntityWish, sinceStr,
			func(w models.Wish) uint { return w.ID },
			func(id uint, deletedAt time.Time) models.Wish {
				return models.Wish{
					ID:        id,
					CreatedAt: deletedAt,
					UpdatedAt: deletedAt,
					DeletedAt: gorm.DeletedAt{Time: deletedAt, Valid: true},
				}
			},
		); err != nil {
			Error(c, http.StatusInternalServerError, "获取心愿列表失败")
			return
		}
	}

	populateWishURLsBatch(wishes)
	if !isAdmin {
		for i := range wishes {
			redactEmbeddedWishFinances(&wishes[i])
		}
	}

	// MOB-P1-36：服务端权威增量游标（保持本页语义）
	setSyncCursor(c, computeSyncCursor(wishes, func(w models.Wish) (time.Time, gorm.DeletedAt) {
		return w.UpdatedAt, w.DeletedAt
	}))

	Success(c, gin.H{
		"total":     agg.Total,
		"page":      page,
		"page_size": pageSize,
		"wishes":    wishes,
	})
}

// GetWishByID 单个心愿详情 (访客模式脱敏 BE-14)
func GetWishByID(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "wishes") {
		return
	}
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	var w models.Wish
	if err := db.DB.First(&w, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}
	isAdmin := middleware.IsAdmin(c)
	if !isAdmin && w.IsPrivate {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}
	populateWishURLs(&w)
	setAuthVaryHeader(c)
	if !isAdmin {
		redactEmbeddedWishFinances(&w)
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

	// 契约 B：幂等创建。同一 client_request_id 的重复提交（重试/重放）直接返回已有记录，
	// 不重复落库；并发窗口由 client_request_id 唯一索引兜底（插入冲突后回读返回）。
	clientRequestID := strings.TrimSpace(req.ClientRequestID)
	if existing, deleted, ok := findIdempotentReplay[models.Wish](clientRequestID, func(w *models.Wish) (uint, bool) {
		return w.ID, w.DeletedAt.Valid
	}); ok {
		replyIdempotentHit(c, existing, deleted, populateWishURLs)
		return
	}

	var targetDate *time.Time
	if req.TargetDate != "" {
		t, err := ParseFlexTime(req.TargetDate)
		if err != nil {
			Error(c, http.StatusBadRequest, "目标日期格式无法识别")
			return
		}
		targetDate = &t
	}

	// 补档：许下时刻可指定为过去（省略则取当前）
	var plantedAt *time.Time
	if req.PlantedAt != "" {
		t, err := ParseFlexTime(req.PlantedAt)
		if err != nil {
			Error(c, http.StatusBadRequest, "许愿时间格式无法识别")
			return
		}
		plantedAt = &t
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
	if !validWishKinds[wishKind] {
		Error(c, http.StatusBadRequest, "kind 仅支持 item / travel / milestone")
		return
	}

	// 预算不能为负（兜底校验修复）：负预算会污染心愿成本口径
	if req.Budget < 0 {
		Error(c, http.StatusBadRequest, "预算不能为负数")
		return
	}

	wish := models.Wish{
		Title:      strings.TrimSpace(req.Title),
		Kind:       wishKind,
		Note:       strings.TrimSpace(req.Note),
		Spec:       strings.TrimSpace(req.Spec),
		Budget:     req.Budget,
		Status:     "wanting",
		TargetDate: targetDate,
		PlantedAt:  plantedAt,
		IsPrivate:  req.IsPrivate,
	}
	if clientRequestID != "" {
		wish.ClientRequestID = &clientRequestID
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&wish).Error; err != nil {
			return err
		}

		companionEntry := models.LifeEntry{
			Type:    "wish",
			Title:   companionWishPlantedTitlePrefix + wish.Title,
			Content: buildWishCompanionContent(wish),
			// 关键：伴生散页落在**实际许下的时刻**，补档的心愿才会出现在时间流的正确位置
			OccurredAt: plantedMoment,
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
			// 归属校验后绑定：抢占其他实体的附件直接 400（发现 15）
			if err := claimAttachments(tx, req.AttachmentIDs, map[string]any{
				"wish_id":  wish.ID,
				"entry_id": companionEntry.ID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// 并发重复提交：唯一索引兜底，回读已有记录按幂等语义返回。
		if existing, deleted, ok := findIdempotentReplay[models.Wish](clientRequestID, func(w *models.Wish) (uint, bool) {
			return w.ID, w.DeletedAt.Valid
		}); ok {
			replyIdempotentHit(c, existing, deleted, populateWishURLs)
			return
		}
		if errors.Is(err, errAttachmentClaimConflict) {
			Error(c, http.StatusBadRequest, "创建心愿失败: "+err.Error())
			return
		}
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
	if err := db.DB.First(&wish, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}

	var req UpdateWishRequest
	rawBytes, _ := io.ReadAll(c.Request.Body)
	if err := json.Unmarshal(rawBytes, &req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}
	// 字段语义与 profile 一致：**请求携带该 key 即按其值写入**（0 = 清空）
	var rawKeys map[string]json.RawMessage
	_ = json.Unmarshal(rawBytes, &rawKeys)

	oldStatus := wish.Status
	oldTitle := wish.Title

	if hasJSONValue(rawKeys, "title") {
		title := strings.TrimSpace(req.Title)
		if title == "" {
			Error(c, http.StatusBadRequest, "心愿标题不能为空")
			return
		}
		wish.Title = title
	}
	if req.Kind != "" {
		kind := strings.TrimSpace(req.Kind)
		if !validWishKinds[kind] {
			Error(c, http.StatusBadRequest, "kind 仅支持 item / travel / milestone")
			return
		}
		wish.Kind = kind
	}
	// 契约 C：可清空文本字段统一「请求携带该 key 即按其值写入（空串 = 清空），
	// 缺省 / null = 保持原值」。原先 `if req.Note != ""` 让「删光备注/规格后保存」静默失败，
	// 而只判断 key 是否存在又会把 JSON null 当成清空。
	if req.Note != "" || hasJSONValue(rawKeys, "note") {
		wish.Note = strings.TrimSpace(req.Note)
	}
	if req.Spec != "" || hasJSONValue(rawKeys, "spec") {
		wish.Spec = strings.TrimSpace(req.Spec)
	}
	// 关键：预算要能改成 0；null 保持原值；负数直接 400（兜底校验修复）。
	if req.Budget != 0 || hasJSONValue(rawKeys, "budget") {
		if req.Budget < 0 {
			Error(c, http.StatusBadRequest, "预算不能为负数")
			return
		}
		wish.Budget = req.Budget
	}
	// 实际花费要能改成 0；null 保持原值；负数直接 400（兜底校验修复）。
	if req.ActualCost != nil {
		if *req.ActualCost < 0 {
			Error(c, http.StatusBadRequest, "实际花费不能为负数")
			return
		}
		wish.ActualCost = *req.ActualCost
	}
	if req.Status != "" {
		status := strings.TrimSpace(req.Status)
		if !validWishStatuses[status] {
			Error(c, http.StatusBadRequest, "status 仅支持 wanting / fulfilled / shelved / relinquished")
			return
		}
		wish.Status = status
	}
	if req.IsPrivate != nil {
		wish.IsPrivate = *req.IsPrivate
	}
	// 契约 C：三个时间字段统一 hasJSONValue 语义 —— 请求携带且空串 = 清空；
	// 缺省 / JSON null = 保持原值。原先 hasKey 只判 key 存在，客户端发 null
	// 会被当成清空（与 profile.go 已修复的同类问题一致，第三轮 P2-3）。
	if hasJSONValue(rawKeys, "target_date") {
		if req.TargetDate == "" {
			wish.TargetDate = nil
		} else if t, err := ParseFlexTime(req.TargetDate); err != nil {
			Error(c, http.StatusBadRequest, "目标日期格式无法识别")
			return
		} else {
			wish.TargetDate = &t
		}
	}
	// 补档：许下时刻可改写为过去的时间；显式空串 = 清空
	if hasJSONValue(rawKeys, "planted_at") {
		if req.PlantedAt == "" {
			wish.PlantedAt = nil
		} else if t, err := ParseFlexTime(req.PlantedAt); err != nil {
			Error(c, http.StatusBadRequest, "许愿时间格式无法识别")
			return
		} else {
			wish.PlantedAt = &t
		}
	}
	if hasJSONValue(rawKeys, "fulfilled_at") {
		if req.FulfilledAt == "" {
			wish.FulfilledAt = nil
		} else if t, err := ParseFlexTime(req.FulfilledAt); err != nil {
			Error(c, http.StatusBadRequest, "达成时间格式无法识别")
			return
		} else {
			wish.FulfilledAt = &t
		}
	}
	if wish.Status == "fulfilled" && wish.FulfilledAt == nil {
		now := config.NowInBeijing()
		wish.FulfilledAt = &now
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		// 若通过直接更新状态标记心愿达成，自动伴生创建一条达成一级记录 (Record-First Architecture，纳入同一事务)
		if wish.Status == "fulfilled" && oldStatus != "fulfilled" {
			fulfilledTime := config.NowInBeijing()
			if wish.FulfilledAt != nil {
				fulfilledTime = *wish.FulfilledAt
			}
			entryContent := strings.TrimSpace(wish.Note)
			if entryContent == "" {
				entryContent = fmt.Sprintf("圆满达成心愿「%s」", wish.Title)
			}
			companionEntry := models.LifeEntry{
				Type:       "wish",
				Title:      companionWishFulfilledTitlePrefix + wish.Title,
				Content:    entryContent,
				OccurredAt: fulfilledTime,
				WishID:     &wish.ID,
			}
			if err := tx.Create(&companionEntry).Error; err != nil {
				return err
			}
			wish.LifeEntryID = &companionEntry.ID
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

		// 契约 A：clear_attachments = true 时先解绑该心愿全部参考图；
		// 若同时携带非空 attachment_ids，则再绑定新列表（先解绑再绑定）。
		// 空数组但没声明 clear_attachments → 视为「本次不携带附件字段」，保持原样。
		if req.ClearAttachments {
			if err := tx.Model(&models.Attachment{}).
				Where("wish_id = ?", wish.ID).
				Update("wish_id", nil).Error; err != nil {
				return err
			}
		}
		if len(req.AttachmentIDs) > 0 {
			if err := tx.Model(&models.Attachment{}).
				Where("wish_id = ? AND id NOT IN ?", wish.ID, req.AttachmentIDs).
				Update("wish_id", nil).Error; err != nil {
				return err
			}

			updates := map[string]any{
				"wish_id": wish.ID,
			}
			if wish.LifeEntryID != nil && *wish.LifeEntryID > 0 {
				updates["entry_id"] = *wish.LifeEntryID
			}
			// 归属校验后绑定：抢占其他实体的附件直接 400（发现 15）
			if err := claimAttachments(tx, req.AttachmentIDs, updates); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errAttachmentClaimConflict) {
			Error(c, http.StatusBadRequest, "更新心愿事务执行失败: "+err.Error())
			return
		}
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
	if err := db.DB.First(&wish, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在")
		return
	}

	var req ResolveWishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求格式无效")
		return
	}

	// 实际花费不能为负（兜底校验修复）：负数会被静默落库成负 ActualCost，
	// 且不满足建流水条件（>0）——「归册成功却无流水」的静默分叉。
	if req.ActualCost < 0 {
		Error(c, http.StatusBadRequest, "实际花费不能为负数")
		return
	}

	// 重复归册守卫（兜底校验修复）：ResolveWish 原先不检查当前状态，
	// 已 fulfilled 的心愿再归册一次会再建一笔流水/再建 gear/再建散页 —— 重复负债。
	if wish.Status == "fulfilled" {
		Error(c, http.StatusBadRequest, "该心愿已归册，不能重复归册")
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

	// 达成时间解析与校验 (必须 <= now，杜绝穿越)
	fulfilledAt := config.NowInBeijing()
	if req.FulfilledAt != "" {
		t, err := ParseFlexTime(req.FulfilledAt)
		if err != nil {
			Error(c, http.StatusBadRequest, "达成时间格式无法识别")
			return
		}
		fulfilledAt = t
	}
	if fulfilledAt.After(config.NowInBeijing()) {
		Error(c, http.StatusBadRequest, "达成时间不能晚于当前时间")
		return
	}

	resType := strings.TrimSpace(req.ResolutionType)
	if resType == "" {
		resType = "exact"
	}
	// 非法归册方式直接 400（表单契约修复）：原先静默钳到 exact 会改写用户意图 ——
	// 用户选「理性拔草」却把字段拼错时，静默 exact 会直接生成一笔支出流水。
	if resType != "exact" && resType != "altered" && resType != "relinquished" {
		Error(c, http.StatusBadRequest, "resolution_type 仅支持 exact / altered / relinquished")
		return
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
			// 不生成支出流水，不产生任何负债
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
					WishID:     &wish.ID,
				}
				if err := tx.Create(&entry).Error; err != nil {
					return err
				}
				wish.LifeEntryID = &entry.ID
				if len(req.AttachmentIDs) > 0 {
					// 归属校验后绑定（发现 15）
					if err := claimAttachments(tx, req.AttachmentIDs, map[string]any{
						"wish_id":  wish.ID,
						"entry_id": entry.ID,
					}); err != nil {
						return err
					}
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

		var createdTx *models.Transaction
		// 联动 1: 账本支出流水 (支持分期信贷联动 RC-53)
		if req.CreateTransaction && req.ActualCost > 0 {
			category := "好物购置"
			if wish.Kind == "travel" {
				category = "娱乐"
			}
			displayTitle := "心愿达成: " + wish.Title
			if resType == "altered" && actualTitle != "" {
				displayTitle = fmt.Sprintf("心愿替代达成: %s (原: %s)", actualTitle, wish.Title)
			}
			paymentMethod := "心愿支出"
			if req.IsInstallment {
				paymentMethod = "分期付款"
				if req.InstallmentChannelCode != "" {
					var ch models.CreditChannel
					if tx.Where("code = ?", req.InstallmentChannelCode).First(&ch).Error == nil && ch.Name != "" {
						paymentMethod = ch.Name
					}
				}
			}
			newTx := models.Transaction{
				Amount:        req.ActualCost,
				Type:          "expense",
				Category:      category,
				Title:         displayTitle,
				Note:          fmt.Sprintf("心愿归册支出 (类别: %s, 方案: %s)", wish.Kind, resType),
				PaymentMethod: paymentMethod,
				WishID:        &wish.ID,
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
			createdTx = &newTx
		}

		if createdTx != nil && req.IsInstallment {
			_, _, err := CreatePlanForTransactionTx(
				tx,
				createdTx,
				req.InstallmentMonths,
				req.InstallmentChannelCode,
				"",
				0,
				"",
				req.RepaymentDay,
				"expense",
			)
			if err != nil {
				return err
			}
		}

		// 联动 2: 时间流散页随笔
		if req.CreateLifeEntry {
			content := strings.TrimSpace(req.EntryContent)
			if content == "" {
				content = fmt.Sprintf("圆满达成心愿「%s」", actualTitle)
			}
			entry := models.LifeEntry{
				Type:       "wish",
				Title:      companionWishFulfilledTitlePrefix + actualTitle,
				Content:    content,
				OccurredAt: fulfilledAt,
				WishID:     &wish.ID,
			}
			if err := tx.Create(&entry).Error; err != nil {
				return err
			}
			wish.LifeEntryID = &entry.ID

			if len(req.AttachmentIDs) > 0 {
				// 归属校验后绑定（发现 15）
				if err := claimAttachments(tx, req.AttachmentIDs, map[string]any{
					"wish_id":  wish.ID,
					"entry_id": entry.ID,
				}); err != nil {
					return err
				}
			}
		}

		// 保存心愿本身
		return tx.Save(&wish).Error
	})

	if err != nil {
		if errors.Is(err, errAttachmentClaimConflict) {
			Error(c, http.StatusBadRequest, "心愿归册事务执行失败: "+err.Error())
			return
		}
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
	if err := db.DB.First(&w, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "心愿不存在或已被删除")
		return
	}

	// 关键（发现 14）：伴生散页清理、关联解绑与心愿删除原先分散在多次独立写入，
	// 中途失败会留下「心愿还在、时间流记录已被删」的半套状态 —— 统一包进同一事务。
	purged := false
	var keysToDelete []string
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		// 先收集伴生散页 id（Unscoped：含此前被单独软删的散页），
		// 它们的附件 entry_id 不能悬挂在即将删除的散页上（第三轮 P2-5）。
		var companionIDs []uint
		if err := tx.Unscoped().Model(&models.LifeEntry{}).Where("wish_id = ?", w.ID).Pluck("id", &companionIDs).Error; err != nil {
			return err
		}

		// 删除心愿时**一并软删除**它在时间流里的伴生散页。
		// 原先只是把 wish_id 置空，导致时间流里留下一条再也点不回心愿的孤儿记录。
		if err := deleteWishCompanionEntries(tx, w.ID); err != nil {
			return err
		}

		if purge {
			// 解绑错误必须上抛：吞掉后事务仍会提交，留下指向已删心愿的逻辑外键。
			if err := tx.Model(&models.Transaction{}).Where("wish_id = ?", w.ID).Update("wish_id", nil).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.Attachment{}).Where("wish_id = ?", w.ID).Update("wish_id", nil).Error; err != nil {
				return err
			}
			// 伴生散页已删：仅归属它们的附件物理清理（媒体在事务提交后删除），
			// 共享附件只解绑 entry_id，杜绝「不可见且永不可清」的永久孤儿
			//（与 DeleteLifeEntry purge 同口径，releaseEntryAttachments 共用）。
			for _, entryID := range companionIDs {
				keys, err := releaseEntryAttachments(tx, entryID, true)
				if err != nil {
					return err
				}
				keysToDelete = append(keysToDelete, keys...)
			}
			// P2-3：心愿本体被物理删除，写墓碑让离线客户端能删掉本地行。
			// 伴生散页走的是软删除（上方 tx.Delete），它们的 deleted_at 已经
			// 会出现在增量 entries 结果里，无需额外墓碑。
			if err := recordSyncTombstone(tx, TombstoneEntityWish, w.ID); err != nil {
				return err
			}
			if err := tx.Unscoped().Delete(&w).Error; err != nil {
				return err
			}
			purged = true
			return nil
		}

		// 软删：伴生散页进回收站后，其附件的 entry_id 一并解绑，避免指向再也
		// 回不来的散页；wish_id 保留 —— 软删心愿在 include_deleted 视角仍能看到参考图。
		for _, entryID := range companionIDs {
			if _, err := releaseEntryAttachments(tx, entryID, false); err != nil {
				return err
			}
		}
		return tx.Delete(&w).Error
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "删除心愿失败: "+err.Error())
		return
	}
	for _, key := range keysToDelete {
		_ = storage.DeleteObject(key)
	}
	// 删除心愿会连带软删除它在时间流里的伴生散页（total_entries 随之变化），
	// 统计缓存必须失效，否则 /stats 继续返回旧计数。
	InvalidateStatsCache()
	if purged {
		Success(c, gin.H{"id": id, "purged": true})
		return
	}
	Success(c, gin.H{"id": id, "deleted": true})
}
