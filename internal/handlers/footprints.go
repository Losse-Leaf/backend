package handlers

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

// footprintTripWindow 计算一次旅程足迹的起止日期窗口（含容差天数）
func footprintTripWindow(f *models.Footprint, padDays int) (time.Time, time.Time) {
	start := time.Date(f.VisitedAt.Year(), f.VisitedAt.Month(), f.VisitedAt.Day(), 0, 0, 0, 0, f.VisitedAt.Location()).AddDate(0, 0, -padDays)
	endBase := f.VisitedAt
	if f.DepartedAt != nil && !f.DepartedAt.IsZero() && f.DepartedAt.After(endBase) {
		endBase = *f.DepartedAt
	}
	end := time.Date(endBase.Year(), endBase.Month(), endBase.Day(), 23, 59, 59, 0, endBase.Location()).AddDate(0, 0, padDays)
	return start, end
}

// autoLinkTransactionFootprint 自动对齐记账记录的城市与所属旅程足迹 (方案 A + B 结合)
func autoLinkTransactionFootprint(tx *models.Transaction) {
	if tx == nil {
		return
	}
	if tx.FootprintID != nil && *tx.FootprintID > 0 {
		if tx.CityCode == "" {
			var fp models.Footprint
			if err := db.DB.First(&fp, *tx.FootprintID).Error; err == nil && fp.ID > 0 {
				tx.CityCode = fp.CityCode
			}
		}
		return
	}
	if fpID, ok := bestFootprintFor(tx); ok {
		idCopy := fpID
		tx.FootprintID = &idCopy
	}
}

// footprintLinkPadDays 账目自动挂载到旅程时的日期容差（前后各放宽几天）
const footprintLinkPadDays = 2

// bestFootprintFor 按「同城 + 旅程窗口（± 容差）+ 取最近的一趟」的规则，
// 找出一条账目应当归属的旅程。
//
// 关键：**自动挂载与启动回填必须共用这一套规则**。
// 否则会出现「老数据按一套口径挂、新数据按另一套口径挂」的割裂 ——
// 旅程花销于是忽高忽低，且没人说得清为什么。
func bestFootprintFor(tx *models.Transaction) (uint, bool) {
	if tx == nil || tx.CityCode == "" {
		return 0, false
	}
	occ := tx.OccurredAt
	if occ.IsZero() {
		occ = time.Now()
	}
	var cityFps []models.Footprint
	db.DB.Where("city_code = ?", tx.CityCode).Order("visited_at DESC").Find(&cityFps)
	for _, cand := range cityFps {
		wStart, wEnd := footprintTripWindow(&cand, footprintLinkPadDays)
		if !occ.Before(wStart) && !occ.After(wEnd) {
			return cand.ID, true
		}
	}
	return 0, false
}

// linkUnlinkedTransactions 把所有「未挂载、且按自动挂载规则能找到归属」的账目补挂上去。
//
// 关键：模糊匹配只用来**挂载**，不用来**直接计入花销**。
// 原先统计口径是「显式挂载 ∪ 同城 ±1 天窗口」，
// 于是同城的无关开销（旅程前一天在本地买的东西）会被算进旅程花销。
// 现在口径收紧成「只统计显式挂载的账目」，
// 模糊匹配的职责回归「把该挂的挂上」—— 历史数据因此不会丢。
//
// 归属规则与自动挂载**完全一致**（见 bestFootprintFor）：
// 同城 + 旅程窗口（± 容差）+ 取最近的一趟。
//
// - Parameter onlyCityCode: 只处理该城市的账目（空 = 全部）
// - Returns: 本次补挂的笔数
func linkUnlinkedTransactions(onlyCityCode string) int {
	q := db.DB.Where("(footprint_id IS NULL OR footprint_id = 0) AND city_code != ''")
	if onlyCityCode != "" {
		q = q.Where("city_code = ?", onlyCityCode)
	}
	var txs []models.Transaction
	if err := q.Find(&txs).Error; err != nil {
		return 0
	}
	linked := 0
	for i := range txs {
		if fpID, ok := bestFootprintFor(&txs[i]); ok {
			db.DB.Model(&models.Transaction{}).
				Where("id = ?", txs[i].ID).
				Update("footprint_id", fpID)
			linked++
		}
	}
	return linked
}

// EnsureFootprintTransactionsLinked 启动时的全量回填。
//
// 关键：这是「统计只认显式挂载」的前提 ——
// 口径收紧后，历史数据必须靠**挂载**才能继续计入旅程花销。
func EnsureFootprintTransactionsLinked() {
	linked := linkUnlinkedTransactions("")
	if linked > 0 {
		fmt.Printf("[Footprint] 启动回填：为 %d 笔历史账目补挂了旅程归属\n", linked)
	}
}

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
	if f.Attachments == nil {
		f.Attachments = []models.Attachment{}
	}
	for i := range f.Attachments {
		f.Attachments[i].URL = storage.SignResourceURL(f.Attachments[i].ObjectKey)
		f.Attachments[i].ThumbnailURL = ThumbnailURLFor(f.Attachments[i].ObjectKey, 0)
	}
}

// populateFootprintURLsBatch 批量填充一组旅程的相片与关联动态计数 (BE-30)
func populateFootprintURLsBatch(footprints []models.Footprint) {
	if len(footprints) == 0 {
		return
	}

	fpIDs := make([]uint, 0, len(footprints))
	entrySetByFP := make(map[uint]map[uint]bool, len(footprints))
	entryFP := make(map[uint]uint)
	for i := range footprints {
		f := &footprints[i]
		if f.ID > 0 {
			fpIDs = append(fpIDs, f.ID)
			entrySetByFP[f.ID] = make(map[uint]bool)
			if f.LifeEntryID != nil && *f.LifeEntryID > 0 {
				entrySetByFP[f.ID][*f.LifeEntryID] = true
				entryFP[*f.LifeEntryID] = f.ID
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
				entryFP[r.ID] = *r.FootprintID
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
			switch {
			case att.FootprintID != nil && *att.FootprintID > 0:
				attByFP[*att.FootprintID] = append(attByFP[*att.FootprintID], att)
			case att.EntryID != nil:
				if fid, ok := entryFP[*att.EntryID]; ok {
					attByFP[fid] = append(attByFP[fid], att)
				}
			}
		}
	}

	for i := range footprints {
		f := &footprints[i]
		f.EntriesCount = len(entrySetByFP[f.ID])
		atts := attByFP[f.ID]
		if atts == nil {
			atts = []models.Attachment{}
		}
		for j := range atts {
			atts[j].URL = storage.SignResourceURL(atts[j].ObjectKey)
			atts[j].ThumbnailURL = ThumbnailURLFor(atts[j].ObjectKey, 0)
		}
		f.Attachments = atts
	}
}

// populateFootprintExpenseStats 为单次旅程计算关联的花销笔数与总额 (仅管理员可见)
func populateFootprintExpenseStats(f *models.Footprint, isAdmin bool) {
	if f == nil || f.ID == 0 || !isAdmin {
		return
	}
	// 关键：口径收紧为「**只统计显式挂载**到本旅程的账目」。
	//
	// 原先是「显式挂载 ∪ 同城 ±1 天窗口内的未挂载账目」——
	// 于是**同城的无关开销**会被算进旅程花销：
	// 旅程前一天在本地买的东西、旅程当天没出门却记的账，全都被算上，
	// 旅程卡片上的「这趟花了多少」于是**虚高**。
	//
	// 模糊匹配并没有被丢掉 —— 它的职责回归「**把该挂的挂上**」：
	// 见 linkUnlinkedTransactions 与启动时的全量回填，
	// 历史数据因此不会凭空少算。
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

// CreateFootprintRequest 录入足迹/旅程请求
type CreateFootprintRequest struct {
	CityCode      string `json:"city_code" binding:"required"`
	CityName      string `json:"city_name" binding:"required"`
	Title         string `json:"title"`
	VisitedAt     string `json:"visited_at" binding:"required"`
	DepartedAt    string `json:"departed_at"`
	Note          string `json:"note"`
	LifeEntryID   *uint  `json:"life_entry_id"`
	WishID        *uint  `json:"wish_id"`
	AttachmentIDs []uint `json:"attachment_ids"`
}

// UpdateFootprintRequest 修改足迹/旅程请求
type UpdateFootprintRequest struct {
	CityCode      string `json:"city_code"`
	CityName      string `json:"city_name"`
	Title         string `json:"title"`
	VisitedAt     string `json:"visited_at"`
	DepartedAt    string `json:"departed_at"`
	Note          string `json:"note"`
	LifeEntryID   *uint  `json:"life_entry_id"`
	WishID        *uint  `json:"wish_id"`
	AttachmentIDs []uint `json:"attachment_ids"`
}

// expandFootprintDateRange 当新的动态发生日期超出当前旅程起止范围（跨天）时自动扩展旅程起止日期
func expandFootprintDateRange(fp *models.Footprint, t time.Time) bool {
	if fp == nil || t.IsZero() {
		return false
	}
	changed := false
	startDay := time.Date(fp.VisitedAt.Year(), fp.VisitedAt.Month(), fp.VisitedAt.Day(), 0, 0, 0, 0, fp.VisitedAt.Location())
	endBase := fp.VisitedAt
	if fp.DepartedAt != nil && !fp.DepartedAt.IsZero() && fp.DepartedAt.After(endBase) {
		endBase = *fp.DepartedAt
	}
	endDay := time.Date(endBase.Year(), endBase.Month(), endBase.Day(), 0, 0, 0, 0, endBase.Location())
	curDay := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())

	if curDay.Before(startDay) {
		if fp.DepartedAt == nil || fp.DepartedAt.IsZero() {
			oldStart := fp.VisitedAt
			fp.DepartedAt = &oldStart
		}
		fp.VisitedAt = t
		changed = true
	} else if curDay.After(endDay) {
		dep := t
		fp.DepartedAt = &dep
		changed = true
	}
	return changed
}

// syncFootprintFromLifeEntry 主流自然下渗与旅程智能归并 (方案 A + B 结合)：
// 1. 若 LifeEntry 已指定 FootprintID，直接挂载至该趟旅程并按需扩展起止日期；
// 2. 若未指定，优先查找同一城市在连续日期窗口 (±7天) 内已有的旅程 Footprint 并自动归并；
// 3. 若无相近旅程，则新建一次旅程 Footprint。
func syncFootprintFromLifeEntry(entry *models.LifeEntry, attachmentIDs []uint, dbTx ...*gorm.DB) {
	if entry == nil || entry.ID == 0 {
		return
	}
	targetDB := db.DB
	if len(dbTx) > 0 && dbTx[0] != nil {
		targetDB = dbTx[0]
	}

	cityCode := strings.TrimSpace(entry.CityCode)
	if cityCode == "" || entry.Type == "reflection" || entry.Type == "wish" {
		return
	}

	cityName := strings.TrimSpace(entry.CityName)
	if cityName == "" {
		cityName = strings.TrimSpace(entry.Location)
	}
	if cityName == "" {
		cityName = cityCode
	}

	noteText := strings.TrimSpace(entry.Content)
	if noteText == "" {
		noteText = strings.TrimSpace(entry.Title)
	}

	visitedAt := entry.OccurredAt
	if visitedAt.IsZero() {
		visitedAt = time.Now()
	}

	var fp models.Footprint

	// 1. 优先检查 entry.FootprintID 显式绑定的旅程 (方案 A)
	if entry.FootprintID != nil && *entry.FootprintID > 0 {
		_ = targetDB.Where("id = ?", *entry.FootprintID).Limit(1).Find(&fp).Error
	}

	// 2. 检查是否已有直接以该 entry 作为首条伴生的 Footprint
	if fp.ID == 0 {
		_ = targetDB.Where("life_entry_id = ?", entry.ID).Limit(1).Find(&fp).Error
	}

	// 3. 同城连续多日动态自动归并为一趟旅程 (方案 B: ±7 天窗口)
	if fp.ID == 0 {
		var cityFps []models.Footprint
		targetDB.Where("city_code = ?", cityCode).Order("visited_at DESC").Find(&cityFps)
		for _, cand := range cityFps {
			wStart, wEnd := footprintTripWindow(&cand, 7)
			if !visitedAt.Before(wStart) && !visitedAt.After(wEnd) {
				fp = cand
				break
			}
		}
	}

	if fp.ID > 0 {
		needSave := false
		if fp.LifeEntryID != nil && *fp.LifeEntryID == entry.ID {
			fp.CityCode = cityCode
			fp.CityName = cityName
			if fp.DepartedAt == nil {
				fp.VisitedAt = visitedAt
			} else {
				expandFootprintDateRange(&fp, visitedAt)
			}
			if noteText != "" {
				fp.Note = noteText
			}
			if fp.Title == "" && strings.TrimSpace(entry.Title) != "" {
				fp.Title = strings.TrimSpace(entry.Title)
			}
			needSave = true
		} else {
			if expandFootprintDateRange(&fp, visitedAt) {
				needSave = true
			}
		}
		if needSave {
			_ = targetDB.Save(&fp).Error
		}
	} else {
		fp = models.Footprint{
			CityCode:    cityCode,
			CityName:    cityName,
			Title:       strings.TrimSpace(entry.Title),
			VisitedAt:   visitedAt,
			Note:        noteText,
			LifeEntryID: &entry.ID,
			WishID:      entry.WishID,
		}
		_ = targetDB.Create(&fp).Error
	}

	if fp.ID > 0 {
		if entry.FootprintID == nil || *entry.FootprintID != fp.ID {
			fpID := fp.ID
			entry.FootprintID = &fpID
			targetDB.Model(&models.LifeEntry{}).Where("id = ?", entry.ID).Update("footprint_id", fp.ID)
		}
		if len(attachmentIDs) > 0 {
			targetDB.Model(&models.Attachment{}).
				Where("id IN ?", attachmentIDs).
				Update("footprint_id", fp.ID)
		} else {
			targetDB.Model(&models.Attachment{}).
				Where("entry_id = ? AND (footprint_id IS NULL OR footprint_id = 0)", entry.ID).
				Update("footprint_id", fp.ID)
		}
	}
}

var syncFootprintLock sync.Mutex

// EnsureEntryFootprintsSynced 确保所有带 city_code 的一级记录均已自然下渗并按同城连续日期归并为旅程
func EnsureEntryFootprintsSynced() {
	if db.DB == nil {
		return
	}
	syncFootprintLock.Lock()
	defer syncFootprintLock.Unlock()

	var unlinkedEntries []models.LifeEntry
	db.DB.Where("city_code != '' AND city_code IS NOT NULL AND type NOT IN ('reflection', 'wish')").
		Where("(footprint_id IS NULL OR footprint_id = 0) AND id NOT IN (SELECT life_entry_id FROM footprints WHERE life_entry_id IS NOT NULL AND deleted_at IS NULL)").
		Order("occurred_at ASC").
		Find(&unlinkedEntries)

	for i := range unlinkedEntries {
		syncFootprintFromLifeEntry(&unlinkedEntries[i], nil)
	}

	// 将已有 LifeEntryID 绑定但 FootprintID 为空的记录补齐 FootprintID
	var unlinkedFps []models.Footprint
	db.DB.Select("id, life_entry_id").
		Where("life_entry_id IS NOT NULL AND life_entry_id > 0").
		Where("life_entry_id IN (SELECT id FROM life_entries WHERE (footprint_id IS NULL OR footprint_id = 0) AND deleted_at IS NULL)").
		Order("visited_at ASC, id ASC").
		Find(&unlinkedFps)
	for _, fp := range unlinkedFps {
		if fp.LifeEntryID != nil && *fp.LifeEntryID > 0 {
			db.DB.Model(&models.LifeEntry{}).
				Where("id = ? AND (footprint_id IS NULL OR footprint_id = 0)", *fp.LifeEntryID).
				Update("footprint_id", fp.ID)
		}
	}
}

// GetFootprints 列表查询足迹记录 (支持 city_code, year, since, ETag)
func GetFootprints(c *gin.Context) {
	cityCode := c.Query("city_code")
	yearStr := c.Query("year")
	sinceStr := c.Query("since")
	includeDeleted := c.Query("include_deleted") == "true" || sinceStr != ""

	dbScope := db.DB
	if includeDeleted {
		dbScope = db.DB.Unscoped()
	}

	query := dbScope.Model(&models.Footprint{}).Order("visited_at DESC, id DESC")

	// MOB-P1-36：增量游标过滤统一走 applySyncSince（边界含等号）
	query = applySyncSince(query, sinceStr)

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

	var footprints []models.Footprint
	if err := query.Find(&footprints).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取足迹列表失败")
		return
	}

	isAdmin := middleware.IsAdmin(c)
	populateFootprintURLsBatch(footprints)
	for i := range footprints {
		populateFootprintExpenseStats(&footprints[i], isAdmin)
	}

	// ETag 与 304 缓存协商
	var maxUpdated time.Time
	for _, f := range footprints {
		if f.UpdatedAt.After(maxUpdated) {
			maxUpdated = f.UpdatedAt
		}
		if f.DeletedAt.Valid && f.DeletedAt.Time.After(maxUpdated) {
			maxUpdated = f.DeletedAt.Time
		}
	}
	etag := fmt.Sprintf(`W/"%d-%d"`, len(footprints), maxUpdated.UnixMilli())
	c.Header("ETag", etag)

	// MOB-P1-36：服务端权威增量游标
	setSyncCursor(c, maxUpdated)

	if match := c.GetHeader("If-None-Match"); match != "" && match == etag {
		c.Status(http.StatusNotModified)
		return
	}

	Success(c, footprints)
}

// GetFootprintByID 查询单条足迹详情
func GetFootprintByID(c *gin.Context) {
	id := c.Param("id")
	var f models.Footprint
	if err := db.DB.First(&f, id).Error; err != nil {
		Error(c, http.StatusNotFound, "足迹记录不存在")
		return
	}
	populateFootprintURLs(&f)
	populateFootprintExpenseStats(&f, middleware.IsAdmin(c))
	Success(c, f)
}

// CreateFootprint 录入足迹/旅程，并自动伴生创建对应日期的一级记录写入主时间流 (Record-First Architecture)
func CreateFootprint(c *gin.Context) {
	var req CreateFootprintRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "城市代码、城市名与到访时间必填")
		return
	}

	visited, err := ParseFlexTime(req.VisitedAt)
	if err != nil {
		visited = time.Now()
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
	title := strings.TrimSpace(req.Title)
	note := strings.TrimSpace(req.Note)
	lifeEntryID := req.LifeEntryID

	// 若调用方已传入关联的 LifeEntryID，检查是否已由主流下渗自动生成过对应足迹，避免重复
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
				db.DB.Model(&models.Attachment{}).
					Where("id IN ?", req.AttachmentIDs).
					Updates(map[string]interface{}{
						"footprint_id": existingFP.ID,
						"entry_id":     *lifeEntryID,
					})
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
			// 伴生创建机制：从足迹入口直接录入时，自动伴生创建一条一级记录 (LifeEntry) 写入主时间流 (保证事务原子性)
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
		}

		if err := tx.Create(&footprint).Error; err != nil {
			return err
		}

		if lifeEntryID != nil && *lifeEntryID > 0 {
			tx.Model(&models.LifeEntry{}).Where("id = ?", *lifeEntryID).Update("footprint_id", footprint.ID)
		}

		if len(req.AttachmentIDs) > 0 {
			updates := map[string]interface{}{
				"footprint_id": footprint.ID,
			}
			if lifeEntryID != nil && *lifeEntryID > 0 {
				updates["entry_id"] = *lifeEntryID
			}
			if err := tx.Model(&models.Attachment{}).
				Where("id IN ?", req.AttachmentIDs).
				Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		Error(c, http.StatusInternalServerError, "保存足迹记录失败: "+err.Error())
		return
	}

	db.DB.First(&footprint, footprint.ID)
	// 关键：统计口径已收紧为「只统计显式挂载的账目」，
	// 所以新建旅程后立刻把同城的未挂载账目补挂上去 ——
	// 否则这些历史账目会从旅程花销里凭空消失。
	linkUnlinkedTransactions(footprint.CityCode)
	populateFootprintURLs(&footprint)
	InvalidateStatsCache()

	Success(c, footprint)
}

// UpdateFootprint 修改足迹/旅程并同步更新伴生的一级记录
func UpdateFootprint(c *gin.Context) {
	id := c.Param("id")
	var footprint models.Footprint
	if err := db.DB.First(&footprint, id).Error; err != nil {
		Error(c, http.StatusNotFound, "足迹记录不存在")
		return
	}

	var req UpdateFootprintRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	if req.CityCode != "" {
		footprint.CityCode = strings.TrimSpace(req.CityCode)
	}
	if req.CityName != "" {
		footprint.CityName = strings.TrimSpace(req.CityName)
	}
	if req.Title != "" {
		footprint.Title = strings.TrimSpace(req.Title)
	}
	if req.VisitedAt != "" {
		if t, err := ParseFlexTime(req.VisitedAt); err == nil {
			footprint.VisitedAt = t
		}
	}
	if req.DepartedAt != "" {
		if t, err := ParseFlexTime(req.DepartedAt); err == nil && !t.IsZero() {
			footprint.DepartedAt = &t
		}
	}
	if req.Note != "" {
		footprint.Note = strings.TrimSpace(req.Note)
	}
	if req.LifeEntryID != nil {
		footprint.LifeEntryID = req.LifeEntryID
	}
	if req.WishID != nil {
		footprint.WishID = req.WishID
	}

	if err := db.DB.Save(&footprint).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新足迹失败")
		return
	}

	// 同步更新伴生的一级记录 (LifeEntry)
	if footprint.LifeEntryID != nil && *footprint.LifeEntryID > 0 {
		var entry models.LifeEntry
		if err := db.DB.First(&entry, *footprint.LifeEntryID).Error; err == nil {
			entry.CityCode = footprint.CityCode
			entry.CityName = footprint.CityName
			entry.Location = footprint.CityName
			entry.OccurredAt = footprint.VisitedAt
			if req.Title != "" {
				entry.Title = footprint.Title
			}
			if req.Note != "" {
				entry.Content = footprint.Note
			}
			_ = db.DB.Save(&entry).Error
		}
	}

	if req.AttachmentIDs != nil {
		// 空数组语义 = 解除该旅程的全部相片绑定
		if len(req.AttachmentIDs) == 0 {
			db.DB.Model(&models.Attachment{}).
				Where("footprint_id = ?", footprint.ID).
				Update("footprint_id", nil)
		} else {
			db.DB.Model(&models.Attachment{}).
				Where("footprint_id = ? AND id NOT IN ?", footprint.ID, req.AttachmentIDs).
				Update("footprint_id", nil)

			updates := map[string]interface{}{
				"footprint_id": footprint.ID,
			}
			if footprint.LifeEntryID != nil && *footprint.LifeEntryID > 0 {
				updates["entry_id"] = *footprint.LifeEntryID
			}
			db.DB.Model(&models.Attachment{}).
				Where("id IN ?", req.AttachmentIDs).
				Updates(updates)
		}
	}

	db.DB.First(&footprint, footprint.ID)
	// 同上：改完旅程（城市/日期变了）也要重新补挂一次
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
	if err := db.DB.First(&f, id).Error; err != nil {
		Error(c, http.StatusNotFound, "足迹不存在或已被删除")
		return
	}

	if purge {
		err := db.DB.Transaction(func(tx *gorm.DB) error {
			tx.Model(&models.Attachment{}).Where("footprint_id = ?", f.ID).Update("footprint_id", nil)
			tx.Model(&models.Wish{}).Where("footprint_id = ?", f.ID).Update("footprint_id", nil)
			// 共享引用保护：先摘除所有条目对该旅程的引用，再删除首生条目 (BE-25)
			tx.Model(&models.LifeEntry{}).Where("footprint_id = ?", f.ID).Update("footprint_id", nil)
			if f.LifeEntryID != nil && *f.LifeEntryID > 0 {
				if err := tx.Unscoped().Where("id = ?", *f.LifeEntryID).Delete(&models.LifeEntry{}).Error; err != nil {
					return err
				}
			}
			return tx.Unscoped().Delete(&f).Error
		})
		if err != nil {
			Error(c, http.StatusInternalServerError, "物理删除失败: "+err.Error())
			return
		}
		InvalidateStatsCache()
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	// 软删除足迹并同步软删除伴生 LifeEntry (BE-05)
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		// 共享引用保护：其余条目不应指向已软删旅程 (BE-25)
		tx.Model(&models.LifeEntry{}).Where("footprint_id = ?", f.ID).Update("footprint_id", nil)
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
// 聚合下沉至 SQL GROUP BY，不再全表载入内存 (BE-31)
func GetFootprintsAtlas(c *gin.Context) {
	var totalFootprints int64
	db.DB.Model(&models.Footprint{}).Count(&totalFootprints)

	type cityAggRow struct {
		CityCode string
		CityName string
		VisitCnt int64
		FirstAt  string
		LatestAt string
	}
	var rows []cityAggRow
	db.DB.Model(&models.Footprint{}).
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

// DrawerCategoryItem 回忆抽屉消费分类统计项
type DrawerCategoryItem struct {
	Name   string `json:"name"`
	Amount int64  `json:"amount"`
}

// processDrawerFootprints 统一处理抽屉足迹相片签名、旅程花销统计并计算首访与最近到访日期
func processDrawerFootprints(footprints []models.Footprint, isAdmin bool) (string, string, []uint) {
	populateFootprintURLsBatch(footprints)
	var firstVisit, latestVisit time.Time
	fpIDs := make([]uint, 0, len(footprints))
	for i, f := range footprints {
		populateFootprintExpenseStats(&footprints[i], isAdmin)
		if f.ID > 0 {
			fpIDs = append(fpIDs, f.ID)
		}
		if firstVisit.IsZero() || f.VisitedAt.Before(firstVisit) {
			firstVisit = f.VisitedAt
		}
		endVisit := f.VisitedAt
		if f.DepartedAt != nil && !f.DepartedAt.IsZero() && f.DepartedAt.After(endVisit) {
			endVisit = *f.DepartedAt
		}
		if latestVisit.IsZero() || endVisit.After(latestVisit) {
			latestVisit = endVisit
		}
	}

	firstVisitStr, latestVisitStr := "", ""
	if !firstVisit.IsZero() {
		firstVisitStr = firstVisit.In(config.BeijingLocation).Format("2006-01-02")
	}
	if !latestVisit.IsZero() {
		latestVisitStr = latestVisit.In(config.BeijingLocation).Format("2006-01-02")
	}
	return firstVisitStr, latestVisitStr, fpIDs
}

// buildDrawerLedgerSummary 统一构建回忆抽屉账单光谱 (RC-81, BE-14, RC-93: 仅管理员执行查询与聚合，访客严格脱敏)
func buildDrawerLedgerSummary(isAdmin bool, fetchTxs func() []models.Transaction) gin.H {
	if !isAdmin {
		return gin.H{
			"total_spent":       0,
			"transaction_count": 0,
			"categories":        []DrawerCategoryItem{},
		}
	}

	txs := fetchTxs()
	var totalSpent int64
	expenseCount := 0
	categoryBreakdown := make(map[string]int64)
	for _, tx := range txs {
		if tx.Type == "expense" {
			expenseCount++
			totalSpent += tx.Amount
			categoryBreakdown[tx.Category] += tx.Amount
		}
	}

	categories := make([]DrawerCategoryItem, 0, len(categoryBreakdown))
	for name, amt := range categoryBreakdown {
		categories = append(categories, DrawerCategoryItem{Name: name, Amount: amt})
	}
	sort.Slice(categories, func(i, j int) bool {
		if categories[i].Amount != categories[j].Amount {
			return categories[i].Amount > categories[j].Amount
		}
		return categories[i].Name < categories[j].Name
	})

	return gin.H{
		"total_spent": totalSpent,
		// 关键：计数与 total_spent **同口径**（只数支出）。
		// 原先用 len(txs) 把**收入**也数了进去 ——
		// 于是「4 笔消费」里混着一笔「二手回血」收入，与旁边的金额对不上。
		"transaction_count": expenseCount,
		"categories":        categories,
	}
}

// GetCityMemoryDrawer 城市回忆抽屉聚合接口 (RC-93 两级并行：履迹简志 + 菲林画廊 + 散页随笔 + 城市账单光谱)
func GetCityMemoryDrawer(c *gin.Context) {
	cityCode := strings.TrimSpace(c.Param("city_code"))
	if cityCode == "" {
		Error(c, http.StatusBadRequest, "城市代码必填")
		return
	}

	isAdmin := middleware.IsAdmin(c)

	// 1. 该城市的 Footprints 列表与相片
	var footprints []models.Footprint
	db.DB.Where("city_code = ?", cityCode).Order("visited_at DESC").Find(&footprints)
	if footprints == nil {
		footprints = []models.Footprint{}
	}

	cityName := ""
	if len(footprints) > 0 {
		cityName = footprints[0].CityName
	}
	cityShort := strings.TrimSuffix(cityName, "市")

	firstVisitStr, latestVisitStr, fpIDs := processDrawerFootprints(footprints, isAdmin)

	// 2. 关联该城市及旅程的 LifeEntries 随笔
	var entries []models.LifeEntry
	dbQuery := db.DB.Order("occurred_at DESC")
	if len(fpIDs) > 0 && cityShort != "" {
		dbQuery = dbQuery.Where("city_code = ? OR footprint_id IN ? OR location LIKE ?", cityCode, fpIDs, "%"+cityShort+"%")
	} else if len(fpIDs) > 0 {
		dbQuery = dbQuery.Where("city_code = ? OR footprint_id IN ?", cityCode, fpIDs)
	} else if cityShort != "" {
		dbQuery = dbQuery.Where("city_code = ? OR location LIKE ?", cityCode, "%"+cityShort+"%")
	} else {
		dbQuery = dbQuery.Where("city_code = ?", cityCode)
	}
	dbQuery.Limit(50).Find(&entries)
	if entries == nil {
		entries = []models.LifeEntry{}
	}

	populateEntryURLsBatch(entries)

	// 3. 关联该城市及旅程的消费账单光谱 (仅管理员查询聚合 RC-81, BE-14)
	ledgerSummary := buildDrawerLedgerSummary(isAdmin, func() []models.Transaction {
		var txs []models.Transaction
		txQuery := db.DB.Where("city_code = ?", cityCode)
		if len(fpIDs) > 0 && cityShort != "" {
			txQuery = db.DB.Where("city_code = ? OR footprint_id IN ? OR note LIKE ? OR title LIKE ?", cityCode, fpIDs, "%"+cityShort+"%", "%"+cityShort+"%")
		} else if len(fpIDs) > 0 {
			txQuery = db.DB.Where("city_code = ? OR footprint_id IN ?", cityCode, fpIDs)
		} else if cityShort != "" {
			txQuery = db.DB.Where("city_code = ? OR note LIKE ? OR title LIKE ?", cityCode, "%"+cityShort+"%", "%"+cityShort+"%")
		}
		txQuery.Find(&txs)
		return txs
	})

	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	Success(c, gin.H{
		"city_code": cityCode,
		"city_name": cityName,
		"visit_stats": gin.H{
			"total_visits":      len(footprints),
			"first_visited_at":  firstVisitStr,
			"latest_visited_at": latestVisitStr,
		},
		"footprints":     footprints,
		"entries":        entries,
		"ledger_summary": ledgerSummary,
	})
}

// ProvinceVisitedCity 省级抽屉中记录的已访问城市简要统计
type ProvinceVisitedCity struct {
	CityCode   string `json:"city_code"`
	CityName   string `json:"city_name"`
	VisitCount int    `json:"visit_count"`
}

// ProvinceAdcodeMap 中国 34 个省级行政区划标准代码到全称的映射 (GB/T 2260)
var ProvinceAdcodeMap = map[string]string{
	"11": "北京市",
	"12": "天津市",
	"13": "河北省",
	"14": "山西省",
	"15": "内蒙古自治区",
	"21": "辽宁省",
	"22": "吉林省",
	"23": "黑龙江省",
	"31": "上海市",
	"32": "江苏省",
	"33": "浙江省",
	"34": "安徽省",
	"35": "福建省",
	"36": "江西省",
	"37": "山东省",
	"41": "河南省",
	"42": "湖北省",
	"43": "湖南省",
	"44": "广东省",
	"45": "广西壮族自治区",
	"46": "海南省",
	"50": "重庆市",
	"51": "四川省",
	"52": "贵州省",
	"53": "云南省",
	"54": "西藏自治区",
	"61": "陕西省",
	"62": "甘肃省",
	"63": "青海省",
	"64": "宁夏回族自治区",
	"65": "新疆维吾尔自治区",
	"71": "台湾省",
	"81": "香港特别行政区",
	"82": "澳门特别行政区",
}

// ProvinceShortNameMap 省级行政区简称/无后缀名称 (供模糊搜索与标题匹配)
var ProvinceShortNameMap = map[string]string{
	"11": "北京",
	"12": "天津",
	"13": "河北",
	"14": "山西",
	"15": "内蒙古",
	"21": "辽宁",
	"22": "吉林",
	"23": "黑龙江",
	"31": "上海",
	"32": "江苏",
	"33": "浙江",
	"34": "安徽",
	"35": "福建",
	"36": "江西",
	"37": "山东",
	"41": "河南",
	"42": "湖北",
	"43": "湖南",
	"44": "广东",
	"45": "广西",
	"46": "海南",
	"50": "重庆",
	"51": "四川",
	"52": "贵州",
	"53": "云南",
	"54": "西藏",
	"61": "陕西",
	"62": "甘肃",
	"63": "青海",
	"64": "宁夏",
	"65": "新疆",
	"71": "台湾",
	"81": "香港",
	"82": "澳门",
}

// GetProvinceMemoryDrawer 省份回忆抽屉聚合接口 (RC-91 / RC-93 两级并行：履迹简志 + 菲林画廊 + 散页随笔 + 省份账单光谱)
func GetProvinceMemoryDrawer(c *gin.Context) {
	adcode := strings.TrimSpace(c.Param("adcode"))
	if len(adcode) < 2 {
		Error(c, http.StatusBadRequest, "省份行政代码格式无效")
		return
	}

	provPrefix := adcode[:2]
	provAdcode := provPrefix + "0000"
	if len(adcode) == 6 {
		provAdcode = adcode
	}

	provName, ok := ProvinceAdcodeMap[provPrefix]
	if !ok {
		provName = "未知省份"
	}
	provShort := ProvinceShortNameMap[provPrefix]
	isAdmin := middleware.IsAdmin(c)

	// 1. 该省的 Footprints 列表与相片
	var footprints []models.Footprint
	db.DB.Where("city_code LIKE ?", provPrefix+"%").Order("visited_at DESC").Find(&footprints)
	if footprints == nil {
		footprints = []models.Footprint{}
	}

	firstVisitStr, latestVisitStr, fpIDs := processDrawerFootprints(footprints, isAdmin)

	cityMap := make(map[string]*ProvinceVisitedCity)
	cityOrder := make([]string, 0)
	for _, f := range footprints {
		if f.CityCode != "" {
			if item, exists := cityMap[f.CityCode]; exists {
				item.VisitCount++
			} else {
				name := f.CityName
				if name == "" {
					name = f.CityCode
				}
				cityMap[f.CityCode] = &ProvinceVisitedCity{
					CityCode:   f.CityCode,
					CityName:   name,
					VisitCount: 1,
				}
				cityOrder = append(cityOrder, f.CityCode)
			}
		}
	}

	visitedCities := make([]ProvinceVisitedCity, 0, len(cityMap))
	for _, code := range cityOrder {
		visitedCities = append(visitedCities, *cityMap[code])
	}
	sort.Slice(visitedCities, func(i, j int) bool {
		if visitedCities[i].VisitCount != visitedCities[j].VisitCount {
			return visitedCities[i].VisitCount > visitedCities[j].VisitCount
		}
		return visitedCities[i].CityCode < visitedCities[j].CityCode
	})

	// 2. 关联该省及旅程的 LifeEntries 随笔
	var entries []models.LifeEntry
	entryQuery := db.DB.Order("occurred_at DESC")
	if len(fpIDs) > 0 && provShort != "" {
		entryQuery = entryQuery.Where("city_code LIKE ? OR footprint_id IN ? OR location LIKE ?", provPrefix+"%", fpIDs, "%"+provShort+"%")
	} else if len(fpIDs) > 0 {
		entryQuery = entryQuery.Where("city_code LIKE ? OR footprint_id IN ?", provPrefix+"%", fpIDs)
	} else if provShort != "" {
		entryQuery = entryQuery.Where("city_code LIKE ? OR location LIKE ?", provPrefix+"%", "%"+provShort+"%")
	} else {
		entryQuery = entryQuery.Where("city_code LIKE ?", provPrefix+"%")
	}
	entryQuery.Limit(50).Find(&entries)
	if entries == nil {
		entries = []models.LifeEntry{}
	}

	populateEntryURLsBatch(entries)

	// 3. 关联该省份及旅程的消费账单光谱 (仅管理员下发 RC-81, RC-91, RC-93)
	ledgerSummary := buildDrawerLedgerSummary(isAdmin, func() []models.Transaction {
		var txs []models.Transaction
		txQuery := db.DB.Where("city_code LIKE ?", provPrefix+"%")
		if len(fpIDs) > 0 && provShort != "" {
			txQuery = db.DB.Where("city_code LIKE ? OR footprint_id IN ? OR note LIKE ? OR title LIKE ?", provPrefix+"%", fpIDs, "%"+provShort+"%", "%"+provShort+"%")
		} else if len(fpIDs) > 0 {
			txQuery = db.DB.Where("city_code LIKE ? OR footprint_id IN ?", provPrefix+"%", fpIDs)
		} else if provShort != "" {
			txQuery = db.DB.Where("city_code LIKE ? OR note LIKE ? OR title LIKE ?", provPrefix+"%", "%"+provShort+"%", "%"+provShort+"%")
		}
		txQuery.Find(&txs)
		return txs
	})

	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	Success(c, gin.H{
		"province_adcode": provAdcode,
		"province_name":   provName,
		"visited_cities":  visitedCities,
		"visit_stats": gin.H{
			"city_count":        len(visitedCities),
			"total_visits":      len(footprints),
			"first_visited_at":  firstVisitStr,
			"latest_visited_at": latestVisitStr,
		},
		"footprints":     footprints,
		"entries":        entries,
		"ledger_summary": ledgerSummary,
	})
}
