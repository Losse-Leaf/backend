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

// populateFootprintURLs 通过纯逻辑外键加载足迹及该趟旅程关联的全部相片并签名转化为可访问链接，同时统计聚合动态数
func populateFootprintURLs(f *models.Footprint) {
	if f == nil {
		return
	}
	if f.ID > 0 {
		var linkedEntryIDs []uint
		if f.LifeEntryID != nil && *f.LifeEntryID > 0 {
			linkedEntryIDs = append(linkedEntryIDs, *f.LifeEntryID)
		}
		var extraIDs []uint
		db.DB.Model(&models.LifeEntry{}).Where("footprint_id = ?", f.ID).Pluck("id", &extraIDs)
		seenID := make(map[uint]bool)
		var allEntryIDs []uint
		for _, eid := range append(linkedEntryIDs, extraIDs...) {
			if eid > 0 && !seenID[eid] {
				seenID[eid] = true
				allEntryIDs = append(allEntryIDs, eid)
			}
		}
		f.EntriesCount = len(allEntryIDs)

		var atts []models.Attachment
		if len(allEntryIDs) > 0 {
			db.DB.Where("footprint_id = ? OR entry_id IN ?", f.ID, allEntryIDs).Order("id ASC").Find(&atts)
		} else {
			db.DB.Where("footprint_id = ?", f.ID).Order("id ASC").Find(&atts)
		}
		f.Attachments = atts
	}
	f.Attachments = populateAttachmentURLs(f.Attachments)
}

// populateFootprintURLsBatch 批量填充一组旅程的相片与关联动态计数 (BE-30)
func populateFootprintURLsBatch(footprints []models.Footprint) {
	if len(footprints) == 0 {
		return
	}

	fpIDs := make([]uint, 0, len(footprints))
	entrySetByFP := make(map[uint]map[uint]bool, len(footprints))
	entryFP := make(map[uint][]uint)
	for i := range footprints {
		f := &footprints[i]
		if f.ID > 0 {
			fpIDs = append(fpIDs, f.ID)
			entrySetByFP[f.ID] = make(map[uint]bool)
			if f.LifeEntryID != nil && *f.LifeEntryID > 0 {
				entrySetByFP[f.ID][*f.LifeEntryID] = true
				entryFP[*f.LifeEntryID] = append(entryFP[*f.LifeEntryID], f.ID)
			}
		}
	}

	if len(fpIDs) > 0 {
		type entryRef struct {
			ID          uint
			FootprintID *uint
		}
		var refs []entryRef
		db.DB.Model(&models.LifeEntry{}).
			Select("id, footprint_id").
			Where("footprint_id IN ?", fpIDs).
			Scan(&refs)
		for _, r := range refs {
			if r.FootprintID != nil && *r.FootprintID > 0 {
				if entrySetByFP[*r.FootprintID] == nil {
					entrySetByFP[*r.FootprintID] = make(map[uint]bool)
				}
				entrySetByFP[*r.FootprintID][r.ID] = true
				entryFP[r.ID] = append(entryFP[r.ID], *r.FootprintID)
			}
		}
	}

	attByFP := make(map[uint][]models.Attachment)
	if len(fpIDs) > 0 {
		entryIDs := make([]uint, 0, len(entryFP))
		for eid := range entryFP {
			entryIDs = append(entryIDs, eid)
		}
		var atts []models.Attachment
		if len(entryIDs) > 0 {
			db.DB.Where("footprint_id IN ? OR entry_id IN ?", fpIDs, entryIDs).Order("id ASC").Find(&atts)
		} else {
			db.DB.Where("footprint_id IN ?", fpIDs).Order("id ASC").Find(&atts)
		}
		for _, att := range atts {
			owners := make(map[uint]bool)
			if att.FootprintID != nil && *att.FootprintID > 0 {
				owners[*att.FootprintID] = true
			}
			if att.EntryID != nil {
				for _, fid := range entryFP[*att.EntryID] {
					owners[fid] = true
				}
			}
			for fid := range owners {
				attByFP[fid] = append(attByFP[fid], att)
			}
		}
	}

	for i := range footprints {
		f := &footprints[i]
		f.EntriesCount = len(entrySetByFP[f.ID])
		f.Attachments = populateAttachmentURLs(attByFP[f.ID])
	}
}

// footprintExpenseStatRow 旅程花销聚合查询的结果行
type footprintExpenseStatRow struct {
	FootprintID      uint
	TotalSpent       int64
	TransactionCount int64
}

// populateFootprintExpenseStats 为单次旅程计算关联的花销笔数与总额 (仅管理员可见)
func populateFootprintExpenseStats(f *models.Footprint, isAdmin bool) {
	if f == nil || f.ID == 0 || !isAdmin {
		return
	}
	var stats struct {
		TotalSpent       int64
		TransactionCount int64
	}
	db.DB.Model(&models.Transaction{}).
		Select("COALESCE(SUM(amount), 0) AS total_spent, COUNT(*) AS transaction_count").
		Where("type = 'expense' AND footprint_id = ?", f.ID).
		Scan(&stats)

	f.TotalSpent = stats.TotalSpent
	f.TransactionCount = int(stats.TransactionCount)
}

// populateFootprintExpenseStatsBatch 用一条 GROUP BY 聚合查询回填一组旅程的花销统计 (仅管理员)。
func populateFootprintExpenseStatsBatch(footprints []models.Footprint, isAdmin bool) {
	if !isAdmin || len(footprints) == 0 {
		return
	}
	ids := make([]uint, 0, len(footprints))
	for i := range footprints {
		if footprints[i].ID > 0 {
			ids = append(ids, footprints[i].ID)
		}
	}
	if len(ids) == 0 {
		return
	}

	var rows []footprintExpenseStatRow
	if err := db.DB.Model(&models.Transaction{}).
		Select("footprint_id, COALESCE(SUM(amount), 0) AS total_spent, COUNT(*) AS transaction_count").
		Where("type = 'expense' AND footprint_id IN ?", ids).
		Group("footprint_id").
		Scan(&rows).Error; err != nil {
		return
	}

	statsByFP := make(map[uint]footprintExpenseStatRow, len(rows))
	for _, row := range rows {
		statsByFP[row.FootprintID] = row
	}
	for i := range footprints {
		row, ok := statsByFP[footprints[i].ID]
		if !ok {
			continue
		}
		footprints[i].TotalSpent = row.TotalSpent
		footprints[i].TransactionCount = int(row.TransactionCount)
	}
}

// CreateFootprintRequest 录入足迹/旅程请求
type CreateFootprintRequest struct {
	CityCode        string `json:"city_code" binding:"required"`
	CityName        string `json:"city_name" binding:"required"`
	Title           string `json:"title"`
	VisitedAt       string `json:"visited_at" binding:"required"`
	DepartedAt      string `json:"departed_at"`
	Note            string `json:"note"`
	LifeEntryID     *uint  `json:"life_entry_id"`
	WishID          *uint  `json:"wish_id"`
	AttachmentIDs   []uint `json:"attachment_ids"`
	ClientRequestID string `json:"client_request_id"`
	IsPrivate       bool   `json:"is_private"`
}

// UpdateFootprintRequest 修改足迹/旅程请求
type UpdateFootprintRequest struct {
	CityCode         string `json:"city_code"`
	CityName         string `json:"city_name"`
	Title            string `json:"title"`
	VisitedAt        string `json:"visited_at"`
	DepartedAt       string `json:"departed_at"`
	Note             string `json:"note"`
	LifeEntryID      *uint  `json:"life_entry_id"`
	WishID           *uint  `json:"wish_id"`
	AttachmentIDs    []uint `json:"attachment_ids"`
	ClearAttachments bool   `json:"clear_attachments"`
	IsPrivate        *bool  `json:"is_private"`
}

// normalizeFootprintCity 足迹城市归一（兜底校验修复）：统一走国标字典解析并采用字典标准全称。
func normalizeFootprintCity(cityCode, cityName string) (string, string) {
	code, name, _ := ResolveCityCodeAndName(cityCode, cityName, "")
	if code != "" {
		if standardName, ok := CodeToCityName[code]; ok {
			name = standardName
		}
	}
	return code, name
}

// GetFootprints 列表查询足迹记录 (支持 city_code, year, since, limit, ETag)
func GetFootprints(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "footprints") {
		return
	}
	cityCode := c.Query("city_code")
	yearStr := c.Query("year")
	sinceStr := c.Query("since")
	includeDeleted := c.Query("include_deleted") == "true" || sinceStr != ""
	if !validateSyncSince(c, sinceStr) || !allowDeletedScope(c, includeDeleted) {
		return
	}

	limit := 0
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		if n, err := strconv.Atoi(rawLimit); err == nil && n > 0 {
			limit = n
		}
	}

	dbScope := db.DB
	if includeDeleted {
		dbScope = db.DB.Unscoped()
	}

	query := dbScope.Model(&models.Footprint{}).Order("visited_at DESC, id DESC")
	query = applySyncSince(query, &models.Footprint{}, sinceStr)

	if cityCode != "" {
		query = query.Where("city_code = ?", cityCode)
	}

	if yearStr != "" {
		if y, err := strconv.Atoi(yearStr); err == nil && y > 1900 && y < 3000 {
			start := time.Date(y, 1, 1, 0, 0, 0, 0, config.BeijingLocation)
			end := start.AddDate(1, 0, 0)
			query = query.Where("visited_at >= ? AND visited_at < ?", start, end)
		}
	}

	if limit > 0 {
		query = query.Limit(limit + 1)
	}

	isAdmin := middleware.IsAdmin(c)
	if !isAdmin {
		query = query.Where("is_private = ?", false)
	}
	role := PublicModuleETagSuffix(isAdmin)
	setAuthVaryHeader(c)

	agg, err := aggregateListQuery(query)
	if err != nil {
		Error(c, http.StatusInternalServerError, "获取足迹列表失败")
		return
	}
	if sinceStr == "" && negotiateListCache(c, listETag(c, agg, role)) {
		return
	}

	var footprints []models.Footprint
	if err := query.Find(&footprints).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取足迹列表失败")
		return
	}

	if limit == 0 && sinceStr != "" {
		if err := injectTombstones(&footprints, TombstoneEntityFootprint, sinceStr,
			func(f models.Footprint) uint { return f.ID },
			func(id uint, deletedAt time.Time) models.Footprint {
				return models.Footprint{
					ID:        id,
					CreatedAt: deletedAt,
					UpdatedAt: deletedAt,
					DeletedAt: gorm.DeletedAt{Time: deletedAt, Valid: true},
				}
			},
		); err != nil {
			Error(c, http.StatusInternalServerError, "获取足迹列表失败")
			return
		}
	}

	hasMore := false
	if limit > 0 {
		if len(footprints) > limit {
			footprints = footprints[:limit]
			hasMore = true
		}
		if footprints == nil {
			footprints = []models.Footprint{}
		}
	}

	populateFootprintURLsBatch(footprints)
	populateFootprintExpenseStatsBatch(footprints, isAdmin)

	setSyncCursor(c, computeSyncCursor(footprints, func(f models.Footprint) (time.Time, gorm.DeletedAt) {
		return f.UpdatedAt, f.DeletedAt
	}))

	if limit > 0 {
		Success(c, gin.H{
			"footprints": footprints,
			"has_more":   hasMore,
		})
		return
	}

	Success(c, footprints)
}

// GetFootprintByID 查询单条足迹详情
func GetFootprintByID(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "footprints") {
		return
	}
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	var f models.Footprint
	if err := db.DB.First(&f, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "足迹记录不存在")
		return
	}
	isAdmin := middleware.IsAdmin(c)
	if !isAdmin && f.IsPrivate {
		Error(c, http.StatusNotFound, "足迹记录不存在")
		return
	}
	setAuthVaryHeader(c)
	populateFootprintURLs(&f)
	populateFootprintExpenseStats(&f, isAdmin)
	Success(c, f)
}

// CreateFootprint 录入足迹/旅程，并自动伴生创建对应日期的一级记录写入主时间流 (Record-First Architecture)
func CreateFootprint(c *gin.Context) {
	var req CreateFootprintRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "城市代码、城市名与到访时间必填")
		return
	}

	clientRequestID := strings.TrimSpace(req.ClientRequestID)
	if existing, deleted, ok := findIdempotentReplay[models.Footprint](clientRequestID, func(f *models.Footprint) (uint, bool) {
		return f.ID, f.DeletedAt.Valid
	}); ok {
		replyIdempotentHit(c, existing, deleted, populateFootprintURLs)
		return
	}

	visited, err := ParseFlexTime(req.VisitedAt)
	if err != nil && strings.TrimSpace(req.VisitedAt) != "" {
		Error(c, http.StatusBadRequest, "到访时间格式无法识别")
		return
	}

	var departedAt *time.Time
	if strings.TrimSpace(req.DepartedAt) != "" {
		if dt, err := ParseFlexTime(req.DepartedAt); err == nil && !dt.IsZero() {
			if dt.Before(visited) {
				dt, visited = visited, dt
			}
			departedAt = &dt
		}
	}

	cityCode := strings.TrimSpace(req.CityCode)
	cityName := strings.TrimSpace(req.CityName)
	cityCode, cityName = normalizeFootprintCity(cityCode, cityName)
	title := strings.TrimSpace(req.Title)
	note := strings.TrimSpace(req.Note)
	lifeEntryID := req.LifeEntryID

	if lifeEntryID != nil && *lifeEntryID > 0 {
		var entry models.LifeEntry
		if err := db.DB.Limit(1).Find(&entry, *lifeEntryID).Error; err == nil && entry.ID > 0 {
			updated := false
			if entry.CityCode == "" {
				entry.CityCode = cityCode
				updated = true
			}
			if entry.CityName == "" {
				entry.CityName = cityName
				updated = true
			}
			if entry.Location == "" {
				entry.Location = cityName
				updated = true
			}
			if updated {
				_ = db.DB.Save(&entry).Error
			}
		}

		var existingFP models.Footprint
		if err := db.DB.Where("life_entry_id = ?", *lifeEntryID).Limit(1).Find(&existingFP).Error; err == nil && existingFP.ID > 0 {
			existingFP.CityCode = cityCode
			existingFP.CityName = cityName
			existingFP.VisitedAt = visited
			if departedAt != nil {
				existingFP.DepartedAt = departedAt
			}
			if title != "" {
				existingFP.Title = title
			}
			if note != "" {
				existingFP.Note = note
			}
			if req.WishID != nil {
				existingFP.WishID = req.WishID
			}
			_ = db.DB.Save(&existingFP).Error
			db.DB.Model(&models.LifeEntry{}).Where("id = ?", *lifeEntryID).Update("footprint_id", existingFP.ID)
			if len(req.AttachmentIDs) > 0 {
				if err := claimAttachments(db.DB, req.AttachmentIDs, map[string]any{
					"footprint_id": existingFP.ID,
					"entry_id":     *lifeEntryID,
				}); err != nil {
					if errors.Is(err, errAttachmentClaimConflict) {
						Error(c, http.StatusBadRequest, "绑定附件失败: "+err.Error())
					} else {
						Error(c, http.StatusInternalServerError, "绑定附件失败: "+err.Error())
					}
					return
				}
			}
			db.DB.First(&existingFP, existingFP.ID)
			populateFootprintURLs(&existingFP)
			InvalidateStatsCache()
			Success(c, existingFP)
			return
		}
	}

	var footprint models.Footprint
	err = db.DB.Transaction(func(tx *gorm.DB) error {
		if lifeEntryID == nil || *lifeEntryID == 0 {
			entryTitle := title
			if entryTitle == "" {
				entryTitle = fmt.Sprintf("行迹 · %s", cityName)
			}
			entryContent := note
			if entryContent == "" {
				entryContent = fmt.Sprintf("踏足%s，在山川图谱中留下一枚足迹印信。", cityName)
			}
			companionEntry := models.LifeEntry{
				Type:       "travel",
				Title:      entryTitle,
				Content:    entryContent,
				OccurredAt: visited,
				Location:   cityName,
				CityCode:   cityCode,
				CityName:   cityName,
				WishID:     req.WishID,
			}
			if err := tx.Create(&companionEntry).Error; err != nil {
				return fmt.Errorf("伴生创建时间流记录失败: %w", err)
			}
			lifeEntryID = &companionEntry.ID
		}

		footprint = models.Footprint{
			CityCode:    cityCode,
			CityName:    cityName,
			Title:       title,
			VisitedAt:   visited,
			DepartedAt:  departedAt,
			Note:        note,
			LifeEntryID: lifeEntryID,
			WishID:      req.WishID,
			IsPrivate:   req.IsPrivate,
		}
		if clientRequestID != "" {
			footprint.ClientRequestID = &clientRequestID
		}

		if err := tx.Create(&footprint).Error; err != nil {
			return err
		}

		if lifeEntryID != nil && *lifeEntryID > 0 {
			tx.Model(&models.LifeEntry{}).Where("id = ?", *lifeEntryID).Update("footprint_id", footprint.ID)
		}

		if len(req.AttachmentIDs) > 0 {
			updates := map[string]any{
				"footprint_id": footprint.ID,
			}
			if lifeEntryID != nil && *lifeEntryID > 0 {
				updates["entry_id"] = *lifeEntryID
			}
			if err := claimAttachments(tx, req.AttachmentIDs, updates); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		if existing, deleted, ok := findIdempotentReplay[models.Footprint](clientRequestID, func(f *models.Footprint) (uint, bool) {
			return f.ID, f.DeletedAt.Valid
		}); ok {
			replyIdempotentHit(c, existing, deleted, populateFootprintURLs)
			return
		}
		if errors.Is(err, errAttachmentClaimConflict) {
			Error(c, http.StatusBadRequest, "保存足迹记录失败: "+err.Error())
			return
		}
		Error(c, http.StatusInternalServerError, "保存足迹记录失败: "+err.Error())
		return
	}

	db.DB.First(&footprint, footprint.ID)
	linkUnlinkedTransactions(footprint.CityCode)
	populateFootprintURLs(&footprint)
	InvalidateStatsCache()

	Success(c, footprint)
}

// UpdateFootprint 修改足迹/旅程并同步更新伴生的一级记录
func UpdateFootprint(c *gin.Context) {
	id := c.Param("id")
	var footprint models.Footprint
	if err := db.DB.First(&footprint, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "足迹记录不存在")
		return
	}

	var req UpdateFootprintRequest
	rawBytes, _ := io.ReadAll(c.Request.Body)
	if err := json.Unmarshal(rawBytes, &req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}
	var rawKeys map[string]json.RawMessage
	_ = json.Unmarshal(rawBytes, &rawKeys)

	oldCityCode := footprint.CityCode
	oldCityName := footprint.CityName
	if req.CityCode != "" {
		footprint.CityCode = strings.TrimSpace(req.CityCode)
	}
	if req.CityName != "" {
		footprint.CityName = strings.TrimSpace(req.CityName)
	}
	if req.CityCode != "" || req.CityName != "" {
		footprint.CityCode, footprint.CityName = normalizeFootprintCity(footprint.CityCode, footprint.CityName)
	}
	cityChanged := footprint.CityCode != oldCityCode || footprint.CityName != oldCityName
	if hasJSONValue(rawKeys, "title") {
		footprint.Title = strings.TrimSpace(req.Title)
	}
	if req.VisitedAt != "" {
		if t, err := ParseFlexTime(req.VisitedAt); err == nil {
			footprint.VisitedAt = t
		} else {
			Error(c, http.StatusBadRequest, "到访时间格式无法识别")
			return
		}
	}
	if trimmedDeparted := strings.TrimSpace(req.DepartedAt); trimmedDeparted != "" {
		t, err := ParseFlexTime(trimmedDeparted)
		if err != nil || t.IsZero() {
			Error(c, http.StatusBadRequest, "返程时间格式无法识别")
			return
		}
		footprint.DepartedAt = &t
	} else if hasJSONValue(rawKeys, "departed_at") {
		footprint.DepartedAt = nil
	}
	if footprint.DepartedAt != nil && !footprint.DepartedAt.IsZero() && footprint.DepartedAt.Before(footprint.VisitedAt) {
		swappedVisited := footprint.VisitedAt
		footprint.VisitedAt = *footprint.DepartedAt
		footprint.DepartedAt = &swappedVisited
	}
	if hasJSONValue(rawKeys, "note") {
		footprint.Note = strings.TrimSpace(req.Note)
	}
	if req.LifeEntryID != nil {
		footprint.LifeEntryID = req.LifeEntryID
	}
	if req.WishID != nil {
		footprint.WishID = req.WishID
	}
	if req.IsPrivate != nil {
		footprint.IsPrivate = *req.IsPrivate
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&footprint).Error; err != nil {
			return err
		}

		if footprint.LifeEntryID != nil && *footprint.LifeEntryID > 0 {
			var entry models.LifeEntry
			if err := tx.First(&entry, *footprint.LifeEntryID).Error; err == nil {
				if cityChanged {
					entry.CityCode = footprint.CityCode
					entry.CityName = footprint.CityName
					if entry.Location == "" || entry.Location == oldCityName {
						entry.Location = footprint.CityName
					}
				}
				entry.OccurredAt = footprint.VisitedAt
				if hasJSONValue(rawKeys, "title") {
					entry.Title = footprint.Title
				}
				if hasJSONValue(rawKeys, "note") {
					entry.Content = footprint.Note
				}
				if err := tx.Save(&entry).Error; err != nil {
					return err
				}
			}
		}

		if req.ClearAttachments || req.AttachmentIDs != nil {
			var entryIDs []uint
			if err := tx.Model(&models.LifeEntry{}).Where("footprint_id = ?", footprint.ID).Pluck("id", &entryIDs).Error; err != nil {
				return err
			}
			if footprint.LifeEntryID != nil && *footprint.LifeEntryID > 0 {
				entryIDs = append(entryIDs, *footprint.LifeEntryID)
			}
			entrySet := make(map[uint]bool, len(entryIDs))
			for _, entryID := range entryIDs {
				entrySet[entryID] = true
			}

			query := tx.Where("footprint_id = ?", footprint.ID)
			if len(entryIDs) > 0 {
				query = query.Or("entry_id IN ?", entryIDs)
			}
			var current []models.Attachment
			if err := query.Find(&current).Error; err != nil {
				return err
			}
			selected := make(map[uint]bool, len(req.AttachmentIDs))
			for _, attachmentID := range req.AttachmentIDs {
				selected[attachmentID] = true
			}
			existing := make(map[uint]bool, len(current))
			var removedIDs, changedEntryIDs []uint
			for _, attachment := range current {
				existing[attachment.ID] = true
				if !selected[attachment.ID] {
					removedIDs = append(removedIDs, attachment.ID)
					if attachment.EntryID != nil && entrySet[*attachment.EntryID] {
						changedEntryIDs = append(changedEntryIDs, *attachment.EntryID)
					}
				}
			}
			if len(removedIDs) > 0 {
				if err := tx.Model(&models.Attachment{}).
					Where("id IN ? AND footprint_id = ?", removedIDs, footprint.ID).
					Update("footprint_id", nil).Error; err != nil {
					return err
				}
				if len(changedEntryIDs) > 0 {
					if err := tx.Model(&models.Attachment{}).
						Where("id IN ? AND entry_id IN ?", removedIDs, changedEntryIDs).
						Update("entry_id", nil).Error; err != nil {
						return err
					}
					if err := tx.Model(&models.LifeEntry{}).Where("id IN ?", changedEntryIDs).
						Update("updated_at", config.NowInBeijing()).Error; err != nil {
						return err
					}
				}
			}

			var newIDs []uint
			for _, attachmentID := range req.AttachmentIDs {
				if !existing[attachmentID] {
					newIDs = append(newIDs, attachmentID)
					existing[attachmentID] = true
				}
			}
			if len(newIDs) > 0 {
				updates := map[string]any{"footprint_id": footprint.ID}
				if footprint.LifeEntryID != nil && *footprint.LifeEntryID > 0 {
					updates["entry_id"] = *footprint.LifeEntryID
				}
				if err := claimAttachments(tx, newIDs, updates); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errAttachmentClaimConflict) {
			Error(c, http.StatusBadRequest, "更新足迹失败: "+err.Error())
			return
		}
		Error(c, http.StatusInternalServerError, "更新足迹失败: "+err.Error())
		return
	}

	db.DB.First(&footprint, footprint.ID)
	linkUnlinkedTransactions(footprint.CityCode)
	populateFootprintURLs(&footprint)
	InvalidateStatsCache()

	Success(c, footprint)
}

// DeleteFootprint 删除足迹 (默认软删除，?purge=true 为物理删除)
func DeleteFootprint(c *gin.Context) {
	id := c.Param("id")
	purge := c.Query("purge") == "true"

	var f models.Footprint
	if err := db.DB.First(&f, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "足迹不存在或已被删除")
		return
	}

	if purge {
		var keysToDelete []string
		err := db.DB.Transaction(func(tx *gorm.DB) error {
			tx.Model(&models.Attachment{}).Where("footprint_id = ?", f.ID).Update("footprint_id", nil)
			tx.Model(&models.Wish{}).Where("footprint_id = ?", f.ID).Update("footprint_id", nil)
			tx.Model(&models.LifeEntry{}).Where("footprint_id = ?", f.ID).Update("footprint_id", nil)
			if f.LifeEntryID != nil && *f.LifeEntryID > 0 {
				keys, err := releaseEntryAttachments(tx, *f.LifeEntryID, true)
				if err != nil {
					return err
				}
				keysToDelete = keys
				if err := tx.Unscoped().Where("id = ?", *f.LifeEntryID).Delete(&models.LifeEntry{}).Error; err != nil {
					return err
				}
				if err := recordSyncTombstone(tx, TombstoneEntityEntry, *f.LifeEntryID); err != nil {
					return err
				}
			}
			if err := recordSyncTombstone(tx, TombstoneEntityFootprint, f.ID); err != nil {
				return err
			}
			return tx.Unscoped().Delete(&f).Error
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
		tx.Model(&models.LifeEntry{}).Where("footprint_id = ?", f.ID).Update("footprint_id", nil)
		if f.LifeEntryID != nil && *f.LifeEntryID > 0 {
			if _, err := releaseEntryAttachments(tx, *f.LifeEntryID, false); err != nil {
				return err
			}
		}
		if err := tx.Delete(&f).Error; err != nil {
			return err
		}
		if f.LifeEntryID != nil && *f.LifeEntryID > 0 {
			if err := tx.Where("id = ?", *f.LifeEntryID).Delete(&models.LifeEntry{}).Error; err != nil {
				return err
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

// AtlasCitySummary 足迹地图城市聚合结构
type AtlasCitySummary struct {
	CityCode        string `json:"city_code"`
	CityName        string `json:"city_name"`
	VisitCount      int    `json:"visit_count"`
	FirstVisitedAt  string `json:"first_visited_at"`
	LatestVisitedAt string `json:"latest_visited_at"`
}

// GetFootprintsAtlas 获取全国足迹汇总 (按市级汇总去过的城市、次数与首次/最近日期)
func GetFootprintsAtlas(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "footprints") {
		return
	}
	isAdmin := middleware.IsAdmin(c)
	setAuthVaryHeader(c)

	var totalFootprints int64
	countQ := db.DB.Model(&models.Footprint{})
	if !isAdmin {
		countQ = countQ.Where("is_private = ?", false)
	}
	countQ.Count(&totalFootprints)

	type cityAggRow struct {
		CityCode string
		CityName string
		VisitCnt int64
		FirstAt  string
		LatestAt string
	}
	var rows []cityAggRow
	aggQ := db.DB.Model(&models.Footprint{})
	if !isAdmin {
		aggQ = aggQ.Where("is_private = ?", false)
	}
	aggQ.
		Select("city_code, MAX(city_name) AS city_name, COUNT(*) AS visit_cnt, MIN(visited_at) AS first_at, MAX(visited_at) AS latest_at").
		Group("city_code").
		Order("latest_at DESC, first_at DESC, city_code ASC").
		Scan(&rows)

	cities := make([]AtlasCitySummary, 0, len(rows))
	for _, r := range rows {
		cityName := strings.TrimSpace(r.CityName)
		if cityName == "" {
			cityName = r.CityCode
		}
		firstAt, _ := ParseFlexTime(r.FirstAt)
		latestAt, _ := ParseFlexTime(r.LatestAt)
		cities = append(cities, AtlasCitySummary{
			CityCode:        r.CityCode,
			CityName:        cityName,
			VisitCount:      int(r.VisitCnt),
			FirstVisitedAt:  firstAt.In(config.BeijingLocation).Format("2006-01-02"),
			LatestVisitedAt: latestAt.In(config.BeijingLocation).Format("2006-01-02"),
		})
	}

	Success(c, gin.H{
		"total_footprints": totalFootprints,
		"total_cities":     len(cities),
		"cities":           cities,
	})
}
