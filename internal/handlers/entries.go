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

// validEntryTypes 生活散页类型白名单（RC-128 四维收敛后的全部合法取值）。
var validEntryTypes = map[string]bool{
	"life":    true,
	"travel":  true,
	"project": true,
	"wish":    true,
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

	if e.ProjectID != nil && *e.ProjectID > 0 {
		var proj models.Project
		if db.DB.Where("id = ?", *e.ProjectID).Limit(1).Find(&proj).RowsAffected > 0 {
			e.Project = &proj
		}
	} else {
		e.Project = nil
	}

	if e.WishID != nil && *e.WishID > 0 {
		var wish models.Wish
		if db.DB.Where("id = ?", *e.WishID).Limit(1).Find(&wish).RowsAffected > 0 {
			e.Wish = &wish
		}
	} else {
		e.Wish = nil
	}
}

// populateEntryURLsBatch 批量填充一组条目的附件、项目与心愿关联 (BE-30)
func populateEntryURLsBatch(entries []models.LifeEntry) {
	if len(entries) == 0 {
		return
	}

	entryIDs := make([]uint, 0, len(entries))
	projectIDs := make([]uint, 0, len(entries))
	wishIDs := make([]uint, 0, len(entries))
	for i := range entries {
		if entries[i].ID > 0 {
			entryIDs = append(entryIDs, entries[i].ID)
		}
		if entries[i].ProjectID != nil && *entries[i].ProjectID > 0 {
			projectIDs = append(projectIDs, *entries[i].ProjectID)
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

	projMap := make(map[uint]models.Project)
	if len(projectIDs) > 0 {
		var projs []models.Project
		db.DB.Where("id IN ?", projectIDs).Find(&projs)
		for _, p := range projs {
			projMap[p.ID] = p
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

		e.Project = nil
		if e.ProjectID != nil && *e.ProjectID > 0 {
			if p, ok := projMap[*e.ProjectID]; ok {
				proj := p
				e.Project = &proj
			}
		}

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
	w.GearID = nil
}

// redactVisitorEntry 对单条访客可见散页剥离私密关联实体并脱敏财务字段。
func redactVisitorEntry(e *models.LifeEntry) {
	if e == nil {
		return
	}
	if e.Project != nil && e.Project.IsPrivate {
		e.Project = nil
		e.ProjectID = nil
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
	Type            string `json:"type"` // life, travel, project, wish
	Title           string `json:"title"`
	Content         string `json:"content" binding:"required"`
	OccurredAt      string `json:"occurred_at"`
	Location        string `json:"location"`
	CityCode        string `json:"city_code"`
	CityName        string `json:"city_name"`
	ProjectID       *uint  `json:"project_id"`
	WishID          *uint  `json:"wish_id"`
	FootprintID     *uint  `json:"footprint_id"`
	AttachmentIDs   []uint `json:"attachment_ids"`
	ClientRequestID string `json:"client_request_id"`
	IsPrivate       bool   `json:"is_private"`
}

// CreateLifeEntry 创建一条新的生活档案记录（支持主流自然下渗点亮足迹与造物伴生去重）
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
	cityCode := strings.TrimSpace(req.CityCode)
	cityName := strings.TrimSpace(req.CityName)

	if req.FootprintID != nil && *req.FootprintID > 0 && cityCode == "" {
		var fp models.Footprint
		if err := db.DB.First(&fp, *req.FootprintID).Error; err == nil && fp.ID > 0 {
			cityCode = fp.CityCode
			if cityName == "" {
				cityName = fp.CityName
			}
		}
	}

	cityCode, cityName, location = ResolveCityCodeAndName(cityCode, cityName, location)
	if cityCode != "" && location == "" {
		location = cityName
	}

	// 若前端在 CreateProject 后立即调用了 CreateLifeEntry，复用刚伴生创建的同项目记录，避免重复入库
	if req.Type == "project" && req.ProjectID != nil && *req.ProjectID > 0 {
		var existing models.LifeEntry
		if err := db.DB.Where("project_id = ? AND type = 'project'", *req.ProjectID).Order("id DESC").Limit(1).Find(&existing).Error; err == nil && existing.ID > 0 {
			keyCompatible := clientRequestID == "" || existing.ClientRequestID == nil || *existing.ClientRequestID == clientRequestID
			if time.Since(existing.CreatedAt) < 10*time.Second && (existing.Title == title || title == "") && keyCompatible {
				reuseErr := db.DB.Transaction(func(tx *gorm.DB) error {
					if clientRequestID != "" && existing.ClientRequestID == nil {
						existing.ClientRequestID = &clientRequestID
					}
					if title != "" {
						existing.Title = title
					}
					if content != "" {
						existing.Content = content
					}
					if req.OccurredAt != "" {
						existing.OccurredAt = occurred
					}
					if location != "" {
						existing.Location = location
					}
					if cityCode != "" {
						existing.CityCode = cityCode
						existing.CityName = cityName
					}
					if req.FootprintID != nil {
						existing.FootprintID = req.FootprintID
					}
					if err := tx.Save(&existing).Error; err != nil {
						return err
					}
					if len(req.AttachmentIDs) > 0 {
						if err := claimAttachments(tx, req.AttachmentIDs, map[string]any{"entry_id": existing.ID}); err != nil {
							return err
						}
					}
					if err := syncFootprintFromLifeEntry(&existing, req.AttachmentIDs, tx); err != nil {
						return err
					}
					return nil
				})
				if reuseErr != nil {
					if errors.Is(reuseErr, errAttachmentClaimConflict) {
						Error(c, http.StatusBadRequest, "绑定附件失败: "+reuseErr.Error())
					} else {
						Error(c, http.StatusInternalServerError, "更新项目伴生记录失败: "+reuseErr.Error())
					}
					return
				}
				db.DB.First(&existing, existing.ID)
				populateEntryURLs(&existing)
				InvalidateStatsCache()
				Success(c, existing)
				return
			}
		}
	}

	entryType := strings.TrimSpace(req.Type)
	if entryType == "" {
		entryType = "life"
	}
	if !validEntryTypes[entryType] {
		Error(c, http.StatusBadRequest, "type 仅支持 life / travel / project / wish")
		return
	}

	if req.ProjectID != nil && *req.ProjectID > 0 && entryType != "project" {
		Error(c, http.StatusBadRequest, "project_id 仅支持 type=project 的散页")
		return
	}
	if req.WishID != nil && *req.WishID > 0 && entryType != "wish" {
		Error(c, http.StatusBadRequest, "wish_id 仅支持 type=wish 的散页")
		return
	}

	if req.ProjectID != nil && *req.ProjectID == 0 {
		req.ProjectID = nil
	}
	if req.WishID != nil && *req.WishID == 0 {
		req.WishID = nil
	}
	if req.FootprintID != nil && *req.FootprintID == 0 {
		req.FootprintID = nil
	}
	if req.ProjectID != nil && *req.ProjectID > 0 {
		var projectCount int64
		db.DB.Unscoped().Model(&models.Project{}).Where("id = ?", *req.ProjectID).Count(&projectCount)
		if projectCount == 0 {
			Error(c, http.StatusBadRequest, "关联的造物不存在")
			return
		}
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

	entry := models.LifeEntry{
		Type:        entryType,
		Title:       title,
		Content:     content,
		OccurredAt:  occurred,
		Location:    location,
		CityCode:    cityCode,
		CityName:    cityName,
		ProjectID:   req.ProjectID,
		WishID:      req.WishID,
		FootprintID: req.FootprintID,
		IsPrivate:   req.IsPrivate,
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
		if err := syncFootprintFromLifeEntry(&entry, req.AttachmentIDs, tx); err != nil {
			return err
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

	// 核心视角动态映射 (4 大纯化形态: daily, travel, project, wish)
	if category != "" && category != "all" {
		switch category {
		case "daily":
			query = query.Where("type = ?", "life")
		case "travel":
			query = query.Where("type = ? OR (city_code != '' AND city_code IS NOT NULL)", "travel")
		case "project":
			query = query.Where("type = ? OR (project_id IS NOT NULL AND project_id > 0)", "project")
		case "wish":
			query = query.Where("type = ? OR (wish_id IS NOT NULL AND wish_id > 0)", "wish")
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
	Title            string `json:"title"`
	Content          string `json:"content"`
	OccurredAt       string `json:"occurred_at"`
	Location         string `json:"location"`
	CityCode         string `json:"city_code"`
	CityName         string `json:"city_name"`
	ProjectID        *uint  `json:"project_id"`
	WishID           *uint  `json:"wish_id"`
	FootprintID      *uint  `json:"footprint_id"`
	AttachmentIDs    []uint `json:"attachment_ids"`
	ClearAttachments bool   `json:"clear_attachments"`
	ClearCity        bool   `json:"clear_city"`
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
			Error(c, http.StatusBadRequest, "type 仅支持 life / travel / project / wish")
			return
		}
		entry.Type = req.Type
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

	if req.ClearCity {
		entry.CityCode = ""
		entry.CityName = ""
		if hasJSONValue(rawKeys, "location") {
			entry.Location = strings.TrimSpace(req.Location)
		}
	} else if hasJSONValue(rawKeys, "location") || hasJSONValue(rawKeys, "city_code") || hasJSONValue(rawKeys, "city_name") ||
		req.Location != "" || req.CityCode != "" || req.CityName != "" {
		curCode := entry.CityCode
		if hasJSONValue(rawKeys, "city_code") || req.CityCode != "" {
			curCode = req.CityCode
		}
		curName := entry.CityName
		if hasJSONValue(rawKeys, "city_name") || req.CityName != "" {
			curName = req.CityName
		}
		curLoc := entry.Location
		if hasJSONValue(rawKeys, "location") || req.Location != "" {
			curLoc = req.Location
		}
		if strings.TrimSpace(curCode) == "" && strings.TrimSpace(curName) == "" && strings.TrimSpace(curLoc) == "" {
			entry.CityCode = ""
			entry.CityName = ""
			entry.Location = ""
		} else {
			cCode, cName, cLoc := ResolveCityCodeAndName(curCode, curName, curLoc)
			entry.CityCode = cCode
			entry.CityName = cName
			entry.Location = cLoc
		}
	}
	if req.FootprintID != nil {
		if *req.FootprintID == 0 {
			req.FootprintID = nil
		} else {
			var fpCount int64
			db.DB.Unscoped().Model(&models.Footprint{}).Where("id = ?", *req.FootprintID).Count(&fpCount)
			if fpCount == 0 {
				Error(c, http.StatusBadRequest, "关联的旅程不存在")
				return
			}
		}
		entry.FootprintID = req.FootprintID
		if req.FootprintID != nil && *req.FootprintID > 0 && entry.CityCode == "" && !req.ClearCity {
			var fp models.Footprint
			if err := db.DB.First(&fp, *req.FootprintID).Error; err == nil && fp.ID > 0 {
				entry.CityCode = fp.CityCode
				if entry.CityName == "" {
					entry.CityName = fp.CityName
				}
			}
		}
	}
	if entry.CityCode != "" && entry.Location == "" && !hasJSONValue(rawKeys, "location") {
		entry.Location = entry.CityName
	}
	if _, present := rawKeys["project_id"]; present {
		if req.ProjectID != nil && *req.ProjectID == 0 {
			req.ProjectID = nil
		}
		if req.ProjectID != nil && *req.ProjectID > 0 {
			var projectCount int64
			db.DB.Unscoped().Model(&models.Project{}).Where("id = ?", *req.ProjectID).Count(&projectCount)
			if projectCount == 0 {
				Error(c, http.StatusBadRequest, "关联的造物不存在")
				return
			}
		}
		entry.ProjectID = req.ProjectID
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

	if req.ProjectID != nil && *req.ProjectID > 0 && entry.Type != "project" {
		Error(c, http.StatusBadRequest, "project_id 仅支持 type=project 的散页")
		return
	}
	if req.WishID != nil && *req.WishID > 0 && entry.Type != "wish" {
		Error(c, http.StatusBadRequest, "wish_id 仅支持 type=wish 的散页")
		return
	}
	if req.Type != "" && req.Type != "project" {
		boundProject := entry.ProjectID
		if _, present := rawKeys["project_id"]; present {
			boundProject = req.ProjectID
		}
		if boundProject != nil && *boundProject > 0 {
			Error(c, http.StatusBadRequest, "切换类型前需显式解绑 project_id（携带 null）")
			return
		}
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
		if err := syncFootprintFromLifeEntry(&entry, req.AttachmentIDs, tx); err != nil {
			return err
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

	if purge {
		var keysToDelete []string
		err := db.DB.Transaction(func(tx *gorm.DB) error {
			keys, err := releaseEntryAttachments(tx, entry.ID, true)
			if err != nil {
				return err
			}
			keysToDelete = append(keysToDelete, keys...)
			var bornFps []models.Footprint
			tx.Where("life_entry_id = ?", entry.ID).Find(&bornFps)
			for _, fp := range bornFps {
				var refCount int64
				tx.Model(&models.LifeEntry{}).
					Where("footprint_id = ? AND id != ?", fp.ID, entry.ID).
					Count(&refCount)
				if refCount == 0 {
					if err := tx.Unscoped().Delete(&models.Footprint{}, fp.ID).Error; err != nil {
						return err
					}
					if err := recordSyncTombstone(tx, TombstoneEntityFootprint, fp.ID); err != nil {
						return err
					}
					if err := tx.Model(&models.Wish{}).Where("footprint_id = ?", fp.ID).Update("footprint_id", nil).Error; err != nil {
						return err
					}
				}
			}
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
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&entry).Error; err != nil {
			return err
		}
		var bornFps []models.Footprint
		tx.Where("life_entry_id = ?", entry.ID).Find(&bornFps)
		for _, fp := range bornFps {
			var refCount int64
			tx.Model(&models.LifeEntry{}).
				Where("footprint_id = ? AND id != ?", fp.ID, entry.ID).
				Count(&refCount)
			if refCount == 0 {
				if err := tx.Delete(&models.Footprint{}, fp.ID).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	InvalidateStatsCache()

	Success(c, gin.H{"id": id, "deleted": true})
}
