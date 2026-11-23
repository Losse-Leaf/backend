package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// validEntryTypes 生活散页类型白名单。
var validEntryTypes = map[string]bool{
	"life": true,
	"wish": true,
}

// normalizeEntryCategory 归一化散页内容分类（与 Vesper Space 四大展区 1:1 对齐：Photography / Travel / Still Life / Motion，空字符串代表默认日常）。
func normalizeEntryCategory(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	switch strings.ToLower(trimmed) {
	case "", "daily", "life", "日常", "日常随笔":
		return "", true
	case "photography", "摄影":
		return "Photography", true
	case "travel", "旅途", "旅行":
		return "Travel", true
	case "still life", "still_life", "still-life", "stilllife", "静物", "器物":
		return "Still Life", true
	case "motion", "video", "影像":
		return "Motion", true
	default:
		return "", false
	}
}

// DeriveTitleFromContent 从正文派生标题（朋友圈式发布：只填正文，标题取首行）
func DeriveTitleFromContent(content string, maxLength int) string {
	if maxLength <= 0 {
		maxLength = 40
	}
	firstLine := content
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		firstLine = content[:idx]
	}
	trimmed := strings.TrimSpace(firstLine)
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	if len(runes) > maxLength {
		return string(runes[:maxLength]) + "…"
	}
	return trimmed
}

func populateEntryURLs(e *models.LifeEntry) {
	if e == nil {
		return
	}
	if e.ID > 0 {
		var atts []models.Attachment
		db.DB.Where("entry_id = ?", e.ID).Order("id ASC").Find(&atts)
		e.Attachments = atts
	}
	e.Attachments = populateAttachmentURLs(e.Attachments)

	if e.WishID != nil && *e.WishID > 0 {
		var wish models.Wish
		if db.DB.Where("id = ?", *e.WishID).Limit(1).Find(&wish).RowsAffected > 0 {
			e.Wish = &wish
		}
	} else {
		e.Wish = nil
	}
}

// populateEntryURLsBatch 批量填充一组条目的附件与心愿关联 (BE-30)
func populateEntryURLsBatch(entries []models.LifeEntry) {
	if len(entries) == 0 {
		return
	}

	entryIDs := make([]uint, 0, len(entries))
	wishIDs := make([]uint, 0, len(entries))
	for i := range entries {
		if entries[i].ID > 0 {
			entryIDs = append(entryIDs, entries[i].ID)
		}
		if entries[i].WishID != nil && *entries[i].WishID > 0 {
			wishIDs = append(wishIDs, *entries[i].WishID)
		}
	}

	attMap := make(map[uint][]models.Attachment)
	if len(entryIDs) > 0 {
		var atts []models.Attachment
		db.DB.Where("entry_id IN ?", entryIDs).Order("id ASC").Find(&atts)
		for _, att := range atts {
			if att.EntryID != nil {
				attMap[*att.EntryID] = append(attMap[*att.EntryID], att)
			}
		}
	}

	wishMap := make(map[uint]models.Wish)
	if len(wishIDs) > 0 {
		var wishes []models.Wish
		db.DB.Where("id IN ?", wishIDs).Find(&wishes)
		for _, w := range wishes {
			wishMap[w.ID] = w
		}
	}

	for i := range entries {
		e := &entries[i]
		e.Attachments = populateAttachmentURLs(attMap[e.ID])

		e.Wish = nil
		if e.WishID != nil && *e.WishID > 0 {
			if w, ok := wishMap[*e.WishID]; ok {
				wish := w
				e.Wish = &wish
			}
		}
	}
}

// redactEmbeddedWishFinances 对公开读接口中心愿实体的财务敏感字段做统一访客脱敏。
func redactEmbeddedWishFinances(w *models.Wish) {
	if w == nil {
		return
	}
	w.ActualCost = 0
	w.TransactionID = nil
}

// redactVisitorEntry 对单条访客可见散页剥离私密关联实体并脱敏财务字段。
func redactVisitorEntry(e *models.LifeEntry) {
	if e == nil {
		return
	}
	if e.Wish != nil && e.Wish.IsPrivate {
		e.Wish = nil
		e.WishID = nil
	}
	redactEmbeddedWishFinances(e.Wish)
}

func redactEmbeddedWishFinancesBatch(entries []models.LifeEntry) {
	for i := range entries {
		redactVisitorEntry(&entries[i])
	}
}

// CreateEntryRequest 新增生活记录请求载荷
type CreateEntryRequest struct {
	Type            string `json:"type"`     // life, wish
	Category        string `json:"category"` // "", Photography, Travel, Still Life, Motion
	Title           string `json:"title"`
	Content         string `json:"content" binding:"required"`
	OccurredAt      string `json:"occurred_at"`
	Location        string `json:"location"`
	WishID          *uint  `json:"wish_id"`
	AttachmentIDs   []uint `json:"attachment_ids"`
	ClientRequestID string `json:"client_request_id"`
	IsPrivate       bool   `json:"is_private"`
}

// CreateLifeEntry 创建一条新的生活档案记录
func CreateLifeEntry(c *gin.Context) {
	var req CreateEntryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "内容必填，且必须指定有效类型")
		return
	}

	occurred, err := ParseFlexTime(req.OccurredAt)
	if err != nil && strings.TrimSpace(req.OccurredAt) != "" {
		Error(c, http.StatusBadRequest, "发生时间格式无法识别")
		return
	}

	clientRequestID := strings.TrimSpace(req.ClientRequestID)
	if existing, deleted, ok := findIdempotentReplay[models.LifeEntry](clientRequestID, func(e *models.LifeEntry) (uint, bool) {
		return e.ID, e.DeletedAt.Valid
	}); ok {
		replyIdempotentHit(c, existing, deleted, populateEntryURLs)
		return
	}

	title := strings.TrimSpace(req.Title)
	content := strings.TrimSpace(req.Content)
	if title == "" {
		title = DeriveTitleFromContent(content, 40)
	}
	location := strings.TrimSpace(req.Location)

	entryType := strings.TrimSpace(req.Type)
	if entryType == "" {
		entryType = "life"
	}
	if !validEntryTypes[entryType] {
		Error(c, http.StatusBadRequest, "type 仅支持 life / wish")
		return
	}

	if req.WishID != nil && *req.WishID > 0 && entryType != "wish" {
		Error(c, http.StatusBadRequest, "wish_id 仅支持 type=wish 的散页")
		return
	}

	if req.WishID != nil && *req.WishID == 0 {
		req.WishID = nil
	}
	if req.WishID != nil && *req.WishID > 0 {
		var wishCount int64
		db.DB.Unscoped().Model(&models.Wish{}).Where("id = ?", *req.WishID).Count(&wishCount)
		if wishCount == 0 {
			Error(c, http.StatusBadRequest, "关联的心愿不存在")
			return
		}
	}

	entryCategory, catOK := normalizeEntryCategory(req.Category)
	if !catOK {
		Error(c, http.StatusBadRequest, "category 仅支持 Photography / Travel / Still Life / Motion 或空")
		return
	}

	entry := models.LifeEntry{
		Type:       entryType,
		Category:   entryCategory,
		Title:      title,
		Content:    content,
		OccurredAt: occurred,
		Location:   location,
		WishID:     req.WishID,
		IsPrivate:  req.IsPrivate,
	}
	if clientRequestID != "" {
		entry.ClientRequestID = &clientRequestID
	}

	err = db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
		if len(req.AttachmentIDs) > 0 {
			if err := claimAttachments(tx, req.AttachmentIDs, map[string]any{"entry_id": entry.ID}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if existing, deleted, ok := findIdempotentReplay[models.LifeEntry](clientRequestID, func(e *models.LifeEntry) (uint, bool) {
			return e.ID, e.DeletedAt.Valid
		}); ok {
			replyIdempotentHit(c, existing, deleted, populateEntryURLs)
			return
		}
		if errors.Is(err, errAttachmentClaimConflict) {
			Error(c, http.StatusBadRequest, "保存记录失败: "+err.Error())
			return
		}
		Error(c, http.StatusInternalServerError, "保存记录失败: "+err.Error())
		return
	}

	db.DB.First(&entry, entry.ID)
	populateEntryURLs(&entry)
	InvalidateStatsCache()
	SyncEntryToVesperSpaceAsync(entry.ID, nil)

	Success(c, entry)
}

// GetLifeEntriesV1 列表查询生活档案 (支持分页与类型筛选)
func GetLifeEntriesV1(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "entries") {
		return
	}
	entryType := c.Query("type")
	year := c.Query("year")
	category := c.Query("category")

	pageRaw := c.DefaultQuery("page", "1")
	page, pageErr := strconv.Atoi(pageRaw)
	if pageErr != nil {
		log.Printf("GetLifeEntriesV1: 非法 page 参数 %q，回退默认值: %v", pageRaw, pageErr)
	}
	pageSizeRaw := c.DefaultQuery("page_size", "50")
	pageSize, pageSizeErr := strconv.Atoi(pageSizeRaw)
	if pageSizeErr != nil {
		log.Printf("GetLifeEntriesV1: 非法 page_size 参数 %q，回退默认值: %v", pageSizeRaw, pageSizeErr)
	}
	if page < 1 {
		page = 1
	}
	if page > 100000 {
		page = 100000
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}

	sinceStr := c.Query("since")
	includeDeleted := c.Query("include_deleted") == "true" || sinceStr != ""
	if !validateSyncSince(c, sinceStr) || !allowDeletedScope(c, includeDeleted) {
		return
	}

	dbScope := db.DB
	if includeDeleted {
		dbScope = db.DB.Unscoped()
	}

	isAdmin := middleware.IsAdmin(c)
	role := PublicModuleETagSuffix(isAdmin)
	setAuthVaryHeader(c)

	query := dbScope.Model(&models.LifeEntry{}).
		Order("occurred_at DESC, id DESC")
	if !isAdmin {
		query = query.Where("is_private = ?", false)
	}

	query = applySyncSince(query, &models.LifeEntry{}, sinceStr)

	if entryType != "" && entryType != "all" {
		query = query.Where("type = ?", entryType)
	}
	if year != "" && year != "all" {
		if y, err := strconv.Atoi(year); err == nil && y > 1900 && y < 3000 {
			startOfYear := time.Date(y, 1, 1, 0, 0, 0, 0, config.BeijingLocation)
			endOfYear := time.Date(y+1, 1, 1, 0, 0, 0, 0, config.BeijingLocation)
			query = query.Where("occurred_at >= ? AND occurred_at < ?", startOfYear, endOfYear)
		}
	}

	// 核心视角与分类动态映射 (daily, wish, Photography, Travel, Still Life, Motion)
	if category != "" && category != "all" {
		switch category {
		case "daily":
			query = query.Where("type = ?", "life")
		case "wish":
			query = query.Where("type = ? OR (wish_id IS NOT NULL AND wish_id > 0)", "wish")
		default:
			if normCat, ok := normalizeEntryCategory(category); ok && normCat != "" {
				query = query.Where("category = ?", normCat)
			}
		}
	}

	if beforeOccurredStr := strings.TrimSpace(c.Query("before_occurred_at")); beforeOccurredStr != "" {
		if beforeTime, err := ParseFlexTime(beforeOccurredStr); err == nil {
			if beforeID, err := strconv.Atoi(c.Query("before_id")); err == nil && beforeID > 0 {
				query = query.Where("(occurred_at < ?) OR (occurred_at = ? AND id < ?)", beforeTime, beforeTime, beforeID)
			} else {
				query = query.Where("occurred_at < ?", beforeTime)
			}
		}
	}

	agg, err := aggregateListQuery(query)
	if err != nil {
		Error(c, http.StatusInternalServerError, "获取生活档案失败")
		return
	}
	if sinceStr == "" && negotiateListCache(c, listETag(c, agg, role)) {
		return
	}

	var entries []models.LifeEntry
	offset := (page - 1) * pageSize
	if err := query.Offset(offset).Limit(pageSize).Find(&entries).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取生活档案失败")
		return
	}

	if page == 1 && sinceStr != "" {
		if err := injectTombstones(&entries, TombstoneEntityEntry, sinceStr,
			func(e models.LifeEntry) uint { return e.ID },
			func(id uint, deletedAt time.Time) models.LifeEntry {
				return models.LifeEntry{
					ID:        id,
					CreatedAt: deletedAt,
					UpdatedAt: deletedAt,
					DeletedAt: gorm.DeletedAt{Time: deletedAt, Valid: true},
				}
			},
		); err != nil {
			Error(c, http.StatusInternalServerError, "获取生活档案失败")
			return
		}
	}

	populateEntryURLsBatch(entries)
	if !isAdmin {
		redactEmbeddedWishFinancesBatch(entries)
	}

	setSyncCursor(c, computeSyncCursor(entries, func(e models.LifeEntry) (time.Time, gorm.DeletedAt) {
		return e.UpdatedAt, e.DeletedAt
	}))

	Success(c, gin.H{
		"total":     agg.Total,
		"page":      page,
		"page_size": pageSize,
		"entries":   entries,
	})
}

// GetOnThisDayEntries 查询往年今日生活档案 (GET /api/v1/entries/on-this-day)
func GetOnThisDayEntries(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "entries") {
		return
	}
	isAdmin := middleware.IsAdmin(c)
	now := time.Now().In(config.BeijingLocation)
	month := int(now.Month())
	day := now.Day()
	if m, err := strconv.Atoi(c.Query("month")); err == nil && m >= 1 && m <= 12 {
		month = m
	}
	if d, err := strconv.Atoi(c.Query("day")); err == nil && d >= 1 && d <= 31 {
		day = d
	}

	type idAndTime struct {
		ID         uint
		OccurredAt time.Time
	}
	startOfCurrentYear := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, config.BeijingLocation)
	var metas []idAndTime
	metaQuery := db.DB.Model(&models.LifeEntry{}).
		Select("id, occurred_at").
		Where("occurred_at < ?", startOfCurrentYear)
	if !isAdmin {
		metaQuery = metaQuery.Where("is_private = ?", false)
	}
	if err := metaQuery.
		Order("occurred_at DESC, id DESC").
		Find(&metas).Error; err != nil {
		Error(c, http.StatusInternalServerError, "查询往年今日记录失败")
		return
	}

	matchedIDs := make([]uint, 0)
	for _, m := range metas {
		occ := m.OccurredAt.In(config.BeijingLocation)
		if occ.Year() < now.Year() && int(occ.Month()) == month && occ.Day() == day {
			matchedIDs = append(matchedIDs, m.ID)
		}
	}

	setAuthVaryHeader(c)
	matched := make([]models.LifeEntry, 0, len(matchedIDs))
	if len(matchedIDs) > 0 {
		db.DB.Where("id IN ?", matchedIDs).Order("occurred_at DESC, id DESC").Find(&matched)
		populateEntryURLsBatch(matched)
		if !isAdmin {
			redactEmbeddedWishFinancesBatch(matched)
		}
	}

	Success(c, gin.H{
		"month":   month,
		"day":     day,
		"entries": matched,
	})
}

// GetLifeEntryByID 获取单条生活档案详情
func GetLifeEntryByID(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "entries") {
		return
	}
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	var entry models.LifeEntry
	if err := db.DB.First(&entry, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该条生活记录")
		return
	}

	isAdmin := middleware.IsAdmin(c)
	if !isAdmin && entry.IsPrivate {
		Error(c, http.StatusNotFound, "未找到该条生活记录")
		return
	}

	setAuthVaryHeader(c)
	populateEntryURLs(&entry)
	if !isAdmin {
		redactVisitorEntry(&entry)
	}

	Success(c, entry)
}

// UpdateEntryRequest 更新生活记录请求载荷（字段皆可选）
type UpdateEntryRequest struct {
	Type             string `json:"type"`
	Category         string `json:"category"`
	Title            string `json:"title"`
	Content          string `json:"content"`
	OccurredAt       string `json:"occurred_at"`
	Location         string `json:"location"`
	WishID           *uint  `json:"wish_id"`
	AttachmentIDs    []uint `json:"attachment_ids"`
	ClearAttachments bool   `json:"clear_attachments"`
	IsPrivate        *bool  `json:"is_private"`
}

// UpdateLifeEntry 更新生活记录
func UpdateLifeEntry(c *gin.Context) {
	id := c.Param("id")
	var entry models.LifeEntry
	if err := db.DB.First(&entry, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该条生活记录")
		return
	}

	var req UpdateEntryRequest
	rawBytes, _ := io.ReadAll(c.Request.Body)
	if err := json.Unmarshal(rawBytes, &req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	var rawKeys map[string]json.RawMessage
	_ = json.Unmarshal(rawBytes, &rawKeys)

	if req.Type != "" {
		if !validEntryTypes[req.Type] {
			Error(c, http.StatusBadRequest, "type 仅支持 life / wish")
			return
		}
		entry.Type = req.Type
	}
	if hasJSONValue(rawKeys, "category") || req.Category != "" {
		normCat, ok := normalizeEntryCategory(req.Category)
		if !ok {
			Error(c, http.StatusBadRequest, "category 仅支持 Photography / Travel / Still Life / Motion 或空")
			return
		}
		entry.Category = normCat
	}
	titleKeyPresent := hasJSONValue(rawKeys, "title")
	if req.Title != "" || titleKeyPresent {
		entry.Title = strings.TrimSpace(req.Title)
	}
	if hasJSONValue(rawKeys, "content") || req.Content != "" {
		entry.Content = strings.TrimSpace(req.Content)
		if entry.Content != "" && !titleKeyPresent &&
			(entry.Title == "" || entry.Title == DeriveTitleFromContent(entry.Content, 40)) {
			entry.Title = DeriveTitleFromContent(entry.Content, 40)
		}
	}
	if req.IsPrivate != nil {
		entry.IsPrivate = *req.IsPrivate
	}

	if hasJSONValue(rawKeys, "location") || req.Location != "" {
		entry.Location = strings.TrimSpace(req.Location)
	}
	if _, present := rawKeys["wish_id"]; present {
		if req.WishID != nil && *req.WishID == 0 {
			req.WishID = nil
		}
		if req.WishID != nil && *req.WishID > 0 {
			var wishCount int64
			db.DB.Unscoped().Model(&models.Wish{}).Where("id = ?", *req.WishID).Count(&wishCount)
			if wishCount == 0 {
				Error(c, http.StatusBadRequest, "关联的心愿不存在")
				return
			}
		}
		entry.WishID = req.WishID
	}

	if req.WishID != nil && *req.WishID > 0 && entry.Type != "wish" {
		Error(c, http.StatusBadRequest, "wish_id 仅支持 type=wish 的散页")
		return
	}
	if req.Type != "" && req.Type != "wish" {
		boundWish := entry.WishID
		if _, present := rawKeys["wish_id"]; present {
			boundWish = req.WishID
		}
		if boundWish != nil && *boundWish > 0 {
			Error(c, http.StatusBadRequest, "切换类型前需显式解绑 wish_id（携带 null）")
			return
		}
	}

	if req.OccurredAt != "" {
		t, err := ParseFlexTime(req.OccurredAt)
		if err != nil {
			Error(c, http.StatusBadRequest, "发生时间格式无法识别")
			return
		}
		entry.OccurredAt = t
	}

	var oldAttIDs []uint
	db.DB.Model(&models.Attachment{}).Where("entry_id = ?", entry.ID).Pluck("id", &oldAttIDs)

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&entry).Error; err != nil {
			return err
		}
		if req.ClearAttachments || req.AttachmentIDs != nil {
			query := tx.Model(&models.Attachment{}).Where("entry_id = ?", entry.ID)
			if len(req.AttachmentIDs) > 0 {
				query = query.Where("id NOT IN ?", req.AttachmentIDs)
			}
			if err := query.Update("entry_id", nil).Error; err != nil {
				return err
			}
		}
		if len(req.AttachmentIDs) > 0 {
			if err := claimAttachments(tx, req.AttachmentIDs, map[string]any{"entry_id": entry.ID}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errAttachmentClaimConflict) {
			Error(c, http.StatusBadRequest, "更新记录失败: "+err.Error())
			return
		}
		Error(c, http.StatusInternalServerError, "更新记录失败: "+err.Error())
		return
	}

	db.DB.First(&entry, entry.ID)
	populateEntryURLs(&entry)
	InvalidateStatsCache()
	SyncEntryToVesperSpaceAsync(entry.ID, diffRemovedAttachmentIDs(oldAttIDs, entry.Attachments))

	Success(c, entry)
}

// DeleteLifeEntry 删除生活记录（支持软删除标记与增量同步 BE-05）
func DeleteLifeEntry(c *gin.Context) {
	id := c.Param("id")
	purge := c.Query("purge") == "true"

	var entry models.LifeEntry
	if err := db.DB.First(&entry, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "记录不存在或已被删除")
		return
	}

	var attIDs []uint
	db.DB.Model(&models.Attachment{}).Where("entry_id = ?", entry.ID).Pluck("id", &attIDs)

	if purge {
		var keysToDelete []string
		err := db.DB.Transaction(func(tx *gorm.DB) error {
			keys, err := releaseEntryAttachments(tx, entry.ID, true)
			if err != nil {
				return err
			}
			keysToDelete = append(keysToDelete, keys...)
			tx.Model(&models.Wish{}).Where("life_entry_id = ?", entry.ID).Update("life_entry_id", nil)
			if err := recordSyncTombstone(tx, TombstoneEntityEntry, entry.ID); err != nil {
				return err
			}
			return tx.Unscoped().Delete(&entry).Error
		})
		if err != nil {
			Error(c, http.StatusInternalServerError, "物理删除失败: "+err.Error())
			return
		}
		for _, key := range keysToDelete {
			_ = storage.DeleteObject(key)
		}
		InvalidateStatsCache()
		DeleteSpaceWorksForAttachmentsAsync(attIDs, false)
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	if err := db.DB.Delete(&entry).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	InvalidateStatsCache()
	DeleteSpaceWorksForAttachmentsAsync(attIDs, true)

	Success(c, gin.H{"id": id, "deleted": true})
}
