package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 统一标准响应结构
type Response struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code:    200,
		Message: "success",
		Data:    data,
	})
}

func Error(c *gin.Context, status int, message string) {
	c.JSON(status, Response{
		Code:    status,
		Message: message,
		Data:    nil,
	})
}

// ParseFlexTime 弹性解析前端传入的各种时间格式 (含 HTML5 datetime-local 标准格式 YYYY-MM-DDTHH:mm)
// 时区契约 (RC-134)：无时区后缀的串一律按中国北京时间解释；带偏移/RFC3339 串按自身偏移解析。
// 返回值统一携带 +08:00 偏移，JSON 序列化为 "2026-10-09T14:20:00+08:00"，前端按北京时间展示零漂移。
func ParseFlexTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return config.NowInBeijing(), nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006.01.02 15:04",
		"2006.01.02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, config.BeijingLocation); err == nil {
			return t.In(config.BeijingLocation), nil
		}
	}
	return config.NowInBeijing(), fmt.Errorf("unable to parse time string: %s", s)
}

// ParseSyncSinceTime 解析增量同步用于比较 updated_at / deleted_at 的时间过滤参数。
// 由于 SQLite 底层驱动对 time.Time 采用本地时区文本字面量存储，
// 且 GORM 自动维护的时间戳为 time.Now() (服务器本地时区)，
// 与数据库审计时间戳比较时必须使用本地时区 Local()，确保在 UTC CI/服务器或任意时区下比对无偏差。
func ParseSyncSinceTime(s string) (time.Time, error) {
	t, err := ParseFlexTime(s)
	if err != nil {
		return t, err
	}
	return t.Local(), nil
}

// populateAttachmentURLs 填充附件的动态临时预签名访问 URL (默认 30 分钟有效)
func populateAttachmentURLs(atts []models.Attachment) []models.Attachment {
	for i := range atts {
		atts[i].URL = storage.SignResourceURL(atts[i].ObjectKey)
		atts[i].ThumbnailURL = ThumbnailURLFor(atts[i].ObjectKey, 0)
	}
	return atts
}

// populateEntryURLs 通过纯逻辑外键加载条目关联的附件与项目，并填充预签名 URL (RC-13)
// DeriveTitleFromContent 从正文派生标题（朋友圈式发布：只填正文，标题取首行）
//
// 关键：这条规则必须在**后端、mac、Web 三端一致**，
// 否则同一条记录在不同端标题会不一样，搜索与索引也对不上。
// 规则：取正文第一行（去掉首尾空白），超过 maxLength 则截断并加省略号。
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
	if e.Attachments == nil {
		e.Attachments = []models.Attachment{}
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
// 列表接口不再逐条查询：附件 / 项目 / 心愿各一次 IN 查询后在内存关联
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
		atts := attMap[e.ID]
		if atts == nil {
			atts = []models.Attachment{}
		}
		e.Attachments = populateAttachmentURLs(atts)

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

// redactEmbeddedWishFinances 对公开读接口里「嵌在散页上的心愿」做与心愿列表
// 同口径的访客脱敏 —— 否则访客绕过 /wishes 直接请求 /entries 就能拿到实付金额。
func redactEmbeddedWishFinances(w *models.Wish) {
	if w == nil {
		return
	}
	w.ActualCost = 0
	w.TransactionID = nil
	w.GearID = nil
}

func redactEmbeddedWishFinancesBatch(entries []models.LifeEntry) {
	for i := range entries {
		redactEmbeddedWishFinances(entries[i].Wish)
	}
}

// parseIDParam 校验路由 :id 为正整数并返回（发现 5）。
// 关键：GORM 的 First(&x, id) 会把**非数字字符串**当作原始 SQL 条件拼进 WHERE
// （实测 First(&entry, "2 OR 1=1") 可命中任意行）。各处查询虽已参数化堵住注入，
// 但跨端统一契约要求非法 id 显式返回 400（而非 404），入口统一在此拦截。
func parseIDParam(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		Error(c, http.StatusBadRequest, "资源 ID 必须为正整数")
		return 0, false
	}
	return id, true
}

// errAttachmentClaimConflict 附件跨实体抢占（发现 15）：
// 待绑定附件已归属其他实体（entry / wish / footprint），拒绝绑定。
var errAttachmentClaimConflict = errors.New("附件已绑定到其他实体")

// claimAttachments 校验归属后把一批附件绑定到当前实体（跨端统一契约：防附件抢占）。
//
// targets 的键只允许 entry_id / wish_id / footprint_id。
// 绑定规则：每个附件的三个归属列必须为 NULL，或恰好等于本次要写入的目标值；
// 已挂到其他实体上的附件一律拒绝（返回 errAttachmentClaimConflict 包装错误），
// 否则一条误传的 attachment_ids 会把别家照片静默「偷走」——原先的
// `WHERE id IN ? AND (entry_id IS NULL OR entry_id = ?)` 弱校验只看本实体列，
// 既不校验另外两个归属列，也把冲突静默跳过（客户端毫无感知）。
func claimAttachments(dbTx *gorm.DB, ids []uint, targets map[string]any) error {
	if len(ids) == 0 || dbTx == nil {
		return nil
	}
	var atts []models.Attachment
	if err := dbTx.Where("id IN ?", ids).Find(&atts).Error; err != nil {
		return err
	}
	byID := make(map[uint]models.Attachment, len(atts))
	for _, att := range atts {
		byID[att.ID] = att
	}
	for _, id := range ids {
		att, ok := byID[id]
		if !ok {
			return fmt.Errorf("附件 #%d 不存在或已被清理", id)
		}
		if err := assertAttachmentBindable(att, targets); err != nil {
			return err
		}
	}
	return dbTx.Model(&models.Attachment{}).Where("id IN ?", ids).Updates(targets).Error
}

// assertAttachmentBindable 单个附件的归属校验：三个归属列各自为 NULL，
// 或恰好等于本次要写入的目标值，否则视为抢占冲突。
func assertAttachmentBindable(att models.Attachment, targets map[string]any) error {
	ownerNames := map[string]string{
		"entry_id":     "生活散页",
		"wish_id":      "心愿",
		"footprint_id": "旅程",
	}
	check := func(col string, cur *uint) error {
		if want, has := targets[col]; has {
			wantID, _ := want.(uint)
			if cur != nil && *cur != wantID {
				return fmt.Errorf("%w：附件 #%d 已绑定到其他%s", errAttachmentClaimConflict, att.ID, ownerNames[col])
			}
			return nil
		}
		if cur != nil {
			return fmt.Errorf("%w：附件 #%d 已绑定到其他%s", errAttachmentClaimConflict, att.ID, ownerNames[col])
		}
		return nil
	}
	if err := check("entry_id", att.EntryID); err != nil {
		return err
	}
	if err := check("wish_id", att.WishID); err != nil {
		return err
	}
	return check("footprint_id", att.FootprintID)
}

// ==========================================
// 1. Life Entry CRUD (生活档案接口)
// ==========================================

// CreateEntryRequest 新增生活记录请求载荷
type CreateEntryRequest struct {
	// MOB-P1-35：不再 binding:"required" —— 省略即默认 life。
	// 客户端（Android/macOS/Web）统一采用「省略等于默认值」的编码约定后，
	// 把 type 标成必填会让**省略 type 的合法请求**直接 400。
	Type          string `json:"type"` // life, travel, project, wish
	Title         string `json:"title"`
	Content       string `json:"content" binding:"required"`
	OccurredAt    string `json:"occurred_at"` // ISO8601 或 "2006-01-02T15:04" 或 "2006-01-02 15:04" 或 "2006.01.02"
	Location      string `json:"location"`
	CityCode      string `json:"city_code"`
	CityName      string `json:"city_name"`
	ProjectID     *uint  `json:"project_id"`
	WishID        *uint  `json:"wish_id"`
	FootprintID   *uint  `json:"footprint_id"`
	AttachmentIDs []uint `json:"attachment_ids"`
	// ClientRequestID 幂等创建标识（跨端统一契约 B）：
	// 客户端为每次创建生成稳定 UUID；同一 id 的重复提交直接返回已有记录（HTTP 200）。
	ClientRequestID string `json:"client_request_id"`
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
		// 不能把无法解析的时间静默改成「现在」：那会悄悄改写用户记录的时间。
		Error(c, http.StatusBadRequest, "发生时间格式无法识别")
		return
	}
	// 契约 B：幂等创建。同一 client_request_id 的重复提交（重试/重放）直接返回已有记录，
	// 不重复落库；并发窗口由 client_request_id 唯一索引兜底（插入冲突后回读返回）。
	clientRequestID := strings.TrimSpace(req.ClientRequestID)
	if clientRequestID != "" {
		var existing models.LifeEntry
		if err := db.DB.Where("client_request_id = ?", clientRequestID).Limit(1).Find(&existing).Error; err == nil && existing.ID > 0 {
			populateEntryURLs(&existing)
			Success(c, existing)
			return
		}
	}
	title := strings.TrimSpace(req.Title)
	content := strings.TrimSpace(req.Content)
	// 朋友圈式发布：客户端可能只填正文不填标题 —— 这里统一从正文首行派生，
	// 保证任何端（mac / Web / Android / 裸 API）落库的标题都一致。
	if title == "" {
		title = DeriveTitleFromContent(content, 40)
	}
	location := strings.TrimSpace(req.Location)
	cityCode := strings.TrimSpace(req.CityCode)
	cityName := strings.TrimSpace(req.CityName)

	// 若明确关联了某次旅程足迹 (FootprintID)，自动补齐该旅程的城市代码与名称
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
			if time.Since(existing.CreatedAt) < 10*time.Second && (existing.Title == title || title == "") {
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
				_ = db.DB.Save(&existing).Error
				if len(req.AttachmentIDs) > 0 {
					// 归属校验 + 绑定：已挂到其他实体的附件直接 400，防「偷照片」（发现 15）
					if err := claimAttachments(db.DB, req.AttachmentIDs, map[string]any{"entry_id": existing.ID}); err != nil {
						if errors.Is(err, errAttachmentClaimConflict) {
							Error(c, http.StatusBadRequest, "绑定附件失败: "+err.Error())
						} else {
							Error(c, http.StatusInternalServerError, "绑定附件失败: "+err.Error())
						}
						return
					}
				}
				syncFootprintFromLifeEntry(&existing, req.AttachmentIDs)
				db.DB.First(&existing, existing.ID)
				populateEntryURLs(&existing)
				InvalidateStatsCache()
				Success(c, existing)
				return
			}
		}
	}

	// MOB-P1-35：省略 type 时回落到默认 life（与客户端默认值保持一致）
	entryType := strings.TrimSpace(req.Type)
	if entryType == "" {
		entryType = "life"
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
	}
	if clientRequestID != "" {
		entry.ClientRequestID = &clientRequestID
	}

	// 事务内原子保存主体、关联附件与下渗足迹
	err = db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
		if len(req.AttachmentIDs) > 0 {
			if err := claimAttachments(tx, req.AttachmentIDs, map[string]any{"entry_id": entry.ID}); err != nil {
				return err
			}
		}
		syncFootprintFromLifeEntry(&entry, req.AttachmentIDs, tx)
		return nil
	})
	if err != nil {
		// 并发重复提交：client_request_id 唯一索引兜底，回读已有记录按幂等语义返回
		//（含已被软删除的记录——该请求确实处理过，不该再建一条）。
		if clientRequestID != "" {
			var existing models.LifeEntry
			if e2 := db.DB.Unscoped().Where("client_request_id = ?", clientRequestID).Limit(1).Find(&existing).Error; e2 == nil && existing.ID > 0 {
				populateEntryURLs(&existing)
				Success(c, existing)
				return
			}
		}
		if errors.Is(err, errAttachmentClaimConflict) {
			Error(c, http.StatusBadRequest, "保存记录失败: "+err.Error())
			return
		}
		Error(c, http.StatusInternalServerError, "保存记录失败: "+err.Error())
		return
	}

	// 重新加载逻辑外键关联
	db.DB.First(&entry, entry.ID)
	populateEntryURLs(&entry)
	InvalidateStatsCache()

	Success(c, entry)
}

// GetLifeEntriesV1 列表查询生活档案 (支持分页与类型筛选)
func GetLifeEntriesV1(c *gin.Context) {
	entryType := c.Query("type")
	year := c.Query("year")
	category := c.Query("category") // 支持 4 大纯化核心形态视角 (daily, travel, project, wish, transaction, all)

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if page < 1 {
		page = 1
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

	// 散页内嵌心愿的实付金额按角色脱敏，ETag 必须带角色盐；
	// Vary 在协商之前写出，避免共享缓存/浏览器角色切换后互相命中 304。
	isAdmin := middleware.IsAdmin(c)
	role := "guest"
	if isAdmin {
		role = "admin"
	}
	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")

	query := dbScope.Model(&models.LifeEntry{}).
		Order("occurred_at DESC, id DESC")

	// MOB-P1-36：增量游标过滤统一走 applySyncSince（边界含等号）
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

	// 核心视角动态映射 (4 大纯化形态: daily, travel, project, wish + transaction；RC-128 收敛后旧类型值已不存在，兼容分支移除)
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
		case "transaction":
			query = query.Where("type = ?", "transaction")
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

	// BE-2：先用一条聚合 SQL 拿到 total / max(updated_at) / max(deleted_at)，
	// 再在 Find 与签名之前完成 ETag/304 协商。304 请求最多只花这一条 SQL。
	agg, err := aggregateListQuery(query)
	if err != nil {
		Error(c, http.StatusInternalServerError, "获取生活档案失败")
		return
	}
	// 按角色脱敏 + 软删除会改变 total：禁用 If-Modified-Since，只走 ETag。
	if negotiateListCache(c, listETag(c, agg, role), time.Time{}) {
		return
	}

	var entries []models.LifeEntry
	offset := (page - 1) * pageSize
	if err := query.Offset(offset).Limit(pageSize).Find(&entries).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取生活档案失败")
		return
	}

	populateEntryURLsBatch(entries)
	if !isAdmin {
		redactEmbeddedWishFinancesBatch(entries)
	}

	// MOB-P1-36：服务端权威增量游标（客户端可直接回传为 ?since=）
	setSyncCursor(c, entriesSyncCursor(entries))

	Success(c, gin.H{
		"total":     agg.Total,
		"page":      page,
		"page_size": pageSize,
		"entries":   entries,
	})
}

// GetOnThisDayEntries 查询往年今日生活档案 (GET /api/v1/entries/on-this-day)
// 性能优化：两阶段轻量扫描，第一阶段仅投影 (id, occurred_at) 索引列定位往年同日 ID，避免全表加载大文本 Content
func GetOnThisDayEntries(c *gin.Context) {
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
	if err := db.DB.Model(&models.LifeEntry{}).
		Select("id, occurred_at").
		Where("occurred_at < ?", startOfCurrentYear).
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

	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	matched := make([]models.LifeEntry, 0, len(matchedIDs))
	if len(matchedIDs) > 0 {
		db.DB.Where("id IN ?", matchedIDs).Order("occurred_at DESC, id DESC").Find(&matched)
		populateEntryURLsBatch(matched)
		if !middleware.IsAdmin(c) {
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
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	var entry models.LifeEntry
	if err := db.DB.First(&entry, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该条生活记录")
		return
	}

	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	populateEntryURLs(&entry)
	if !middleware.IsAdmin(c) {
		redactEmbeddedWishFinances(entry.Wish)
	}

	Success(c, entry)
}

// UpdateEntryRequest 更新生活记录请求载荷（字段皆可选）
type UpdateEntryRequest struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	OccurredAt    string `json:"occurred_at"`
	Location      string `json:"location"`
	CityCode      string `json:"city_code"`
	CityName      string `json:"city_name"`
	ProjectID     *uint  `json:"project_id"`
	WishID        *uint  `json:"wish_id"`
	FootprintID   *uint  `json:"footprint_id"`
	AttachmentIDs []uint `json:"attachment_ids"`
	// ClearAttachments 显式解绑该散页全部附件（跨端统一契约 A）：
	// true 时先解绑全部；若同时携带 attachment_ids，则再绑定新列表。
	ClearAttachments bool `json:"clear_attachments"`
	// ClearCity 显式清空城市归属（跨端统一契约 A）：优先级高于
	// city_code / city_name / location 字段本身（携带也忽略）。
	ClearCity bool `json:"clear_city"`
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

	// 关键：字段语义与 profile 更新一致 —— **请求携带该 key 即按其值写入**
	//（空串 = 清空），不再把「空串」当作「未携带」。
	// 原先 `if req.Title != ""` 的写法让用户把标题删干净后保存时
	// **标题仍是旧值**，界面毫无提示 —— 典型的静默失败。
	var rawKeys map[string]json.RawMessage
	_ = json.Unmarshal(rawBytes, &rawKeys)
	hasKey := func(k string) bool {
		_, ok := rawKeys[k]
		return ok
	}

	// type 是**筛选维度**，不是可清空字段。
	// 关键：绝不能用「携带即写入」的语义 ——
	// 一旦某个客户端发出 `"type": ""`，条目会被写成空类型，
	// 从所有分类视角里**静默消失**。
	// 保持旧语义（空串 = 未携带），并对非法值直接拒绝。
	if req.Type != "" {
		entry.Type = req.Type
	}
	// titleKeyPresent：请求是否显式携带了 title
	titleKeyPresent := hasKey("title")
	if req.Title != "" || titleKeyPresent {
		entry.Title = strings.TrimSpace(req.Title)
	}
	// content 同样采用「携带 key 即写入」：空串 = 清空正文。
	// 原先 `if req.Content != ""` 让「删光正文后保存」静默失败。
	if hasKey("content") || req.Content != "" {
		entry.Content = strings.TrimSpace(req.Content)
		// 标题原本就是从正文首行派生的（朋友圈式发布）→ 正文改了，标题跟着改。
		// 关键：**只有请求未携带 title 时**才允许回填派生标题。
		if entry.Content != "" && !titleKeyPresent &&
			(entry.Title == "" || entry.Title == DeriveTitleFromContent(entry.Content, 40)) {
			entry.Title = DeriveTitleFromContent(entry.Content, 40)
		}
	}
	// 契约 A：clear_city 显式清空城市归属，优先级高于 city_code / city_name /
	// location 字段本身（携带也被忽略）；不清空时才走字段驱动的属地解析。
	if req.ClearCity {
		entry.CityCode = ""
		entry.CityName = ""
		entry.Location = ""
	} else if hasKey("location") || hasKey("city_code") || hasKey("city_name") ||
		req.Location != "" || req.CityCode != "" || req.CityName != "" {
		curCode := entry.CityCode
		if hasKey("city_code") || req.CityCode != "" {
			curCode = req.CityCode
		}
		curName := entry.CityName
		if hasKey("city_name") || req.CityName != "" {
			curName = req.CityName
		}
		curLoc := entry.Location
		if hasKey("location") || req.Location != "" {
			curLoc = req.Location
		}
		if strings.TrimSpace(curCode) == "" && strings.TrimSpace(curName) == "" && strings.TrimSpace(curLoc) == "" {
			// 显式清空属地：不再走城市解析（解析器会从标题里“猜”回一个城市）
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
		entry.FootprintID = req.FootprintID
		// clear_city 优先级最高：显式清空属地时不允许再从旅程回填城市（契约 A）
		if *req.FootprintID > 0 && entry.CityCode == "" && !req.ClearCity {
			var fp models.Footprint
			if err := db.DB.First(&fp, *req.FootprintID).Error; err == nil && fp.ID > 0 {
				entry.CityCode = fp.CityCode
				if entry.CityName == "" {
					entry.CityName = fp.CityName
				}
			}
		}
	}
	if entry.CityCode != "" && entry.Location == "" {
		entry.Location = entry.CityName
	}
	// 指针字段：携带 key 才写（null = 解绑）
	if hasKey("project_id") {
		entry.ProjectID = req.ProjectID
	}
	if hasKey("wish_id") {
		entry.WishID = req.WishID
	}

	if req.OccurredAt != "" {
		t, err := ParseFlexTime(req.OccurredAt)
		if err != nil {
			Error(c, http.StatusBadRequest, "发生时间格式无法识别")
			return
		}
		entry.OccurredAt = t
	}

	// 事务内原子保存记录、更新附件关联与下渗足迹
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&entry).Error; err != nil {
			return err
		}
		if req.ClearAttachments {
			// 契约 A：显式解绑全部附件；若同时携带 attachment_ids，则下面再绑定新列表
			if err := tx.Model(&models.Attachment{}).
				Where("entry_id = ?", entry.ID).
				Update("entry_id", nil).Error; err != nil {
				return err
			}
		} else if req.AttachmentIDs != nil {
			// 空数组语义 = 解除该条目的全部附件绑定（不可退化为 NOT IN 空集，那样解绑一条都不发生）
			if len(req.AttachmentIDs) == 0 {
				if err := tx.Model(&models.Attachment{}).
					Where("entry_id = ?", entry.ID).
					Update("entry_id", nil).Error; err != nil {
					return err
				}
			} else {
				// 解除未包含的附件
				if err := tx.Model(&models.Attachment{}).
					Where("entry_id = ? AND id NOT IN ?", entry.ID, req.AttachmentIDs).
					Update("entry_id", nil).Error; err != nil {
					return err
				}
			}
		}
		if len(req.AttachmentIDs) > 0 {
			// 绑定新选中的附件：归属校验失败（抢占其他实体的附件）直接 400（发现 15）
			if err := claimAttachments(tx, req.AttachmentIDs, map[string]any{"entry_id": entry.ID}); err != nil {
				return err
			}
		}
		syncFootprintFromLifeEntry(&entry, req.AttachmentIDs, tx)
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
		// 媒体删除（本地磁盘 / R2 网络）放到事务提交之后：
		// 1) 不在持有 SQLite 写锁时做网络 I/O；
		// 2) 不会出现「对象已删、数据库事务回滚」留下坏链接。
		var keysToDelete []string
		err := db.DB.Transaction(func(tx *gorm.DB) error {
			// 物理硬清理关联的 R2 存储对象与数据库记录（纯逻辑外键下仅删除未与其他实体共用的附件）
			var atts []models.Attachment
			tx.Where("entry_id = ?", entry.ID).Find(&atts)
			for _, att := range atts {
				if att.WishID == nil && att.FootprintID == nil {
					if att.ObjectKey != "" {
						keysToDelete = append(keysToDelete, att.ObjectKey)
					}
					tx.Unscoped().Delete(&att)
				} else {
					tx.Model(&att).Update("entry_id", nil)
				}
			}
			// 仅删除由该条目首生、且无其他条目经 footprint_id 引用的足迹 (RC-25 共享引用保护)
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
				}
			}
			tx.Model(&models.Wish{}).Where("life_entry_id = ?", entry.ID).Update("life_entry_id", nil)
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

	// 软删除落库，支持多端增量同步感知与离线仲裁，并同步软删除下渗足迹 (BE-05)
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&entry).Error; err != nil {
			return err
		}
		// 仅软删除由该条目首生、且无其他存活条目经 footprint_id 引用的足迹
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

// ==========================================
// 2. Uploads & Storage (R2 媒体上传接口)
// ==========================================

type PresignRequest struct {
	Filename    string `json:"filename" binding:"required"`
	ContentType string `json:"content_type" binding:"required"`
	Size        int64  `json:"size" binding:"required"`
}

// PresignUpload 生成 Presigned PUT URL
func PresignUpload(c *gin.Context) {
	var req PresignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少必要的文件信息 (filename, content_type, size)")
		return
	}

	// 大小限制：单文件最大 50MB (与直传接口保持一致)
	maxSize := int64(50 * 1024 * 1024)
	if req.Size > maxSize {
		Error(c, http.StatusBadRequest, "文件过大，单文件上传限制 50MB")
		return
	}

	// MIME 类型基本检查
	ct := strings.ToLower(req.ContentType)
	if !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "video/") && ct != "application/pdf" {
		Error(c, http.StatusBadRequest, "目前仅支持上传图片或媒体文件")
		return
	}

	uploadID, objectKey, presignedURL, expiresIn, err := storage.GeneratePresignedPutURL(req.Filename, req.ContentType, req.Size)
	if err != nil {
		Error(c, http.StatusServiceUnavailable, err.Error())
		return
	}

	Success(c, gin.H{
		"upload_id":  uploadID,
		"object_key": objectKey,
		"upload_url": presignedURL,
		"expires_in": expiresIn,
	})
}

type CompleteUploadRequest struct {
	UploadID         string `json:"upload_id"`
	ObjectKey        string `json:"object_key" binding:"required"`
	OriginalFilename string `json:"original_filename"`
	MimeType         string `json:"mime_type"`
	Size             int64  `json:"size"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
}

// CompleteUpload 确认上传完成并创建 Attachment 实体
func CompleteUpload(c *gin.Context) {
	var req CompleteUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少 object_key")
		return
	}

	// 消费并校验 upload_id 与目标资源路径特征，杜绝凭据越权
	cleanUploadID := strings.TrimSpace(req.UploadID)
	if cleanUploadID != "" && len(cleanUploadID) >= 8 {
		if !strings.Contains(req.ObjectKey, cleanUploadID[:8]) {
			Error(c, http.StatusBadRequest, "上传凭证 upload_id 与目标资源路径不匹配")
			return
		}
	}

	// 如果 R2 已配置，验证对象确实存在于存储桶中
	if storage.IsConfigured() {
		realSize, realContentType, err := storage.VerifyObjectExists(req.ObjectKey)
		if err != nil {
			Error(c, http.StatusBadRequest, "存储端未检测到上传的文件，请重试")
			return
		}
		if realSize > 0 {
			req.Size = realSize
		}
		if realContentType != "" {
			req.MimeType = realContentType
		}

		// 「本地磁盘为主」策略下，预签名直传的字节是客户端直接 PUT 到 R2 的，
		// 服务器没经过那份数据 —— 必须在此刻把它同步回本地，
		// 否则这份文件永远只存在于 R2，「本地为主」对它就不成立。
		if storage.LocalPrimaryEnabled() {
			if err := storage.MirrorObjectToLocal(c.Request.Context(), req.ObjectKey); err != nil {
				// 同步失败不阻断：此时文件仍在 R2，读取时会自动回落
				log.Printf("[storage] 直传对象同步回本地失败（将回落 R2 读取）：key=%s err=%v", req.ObjectKey, err)
			}
		}
	}

	att := models.Attachment{
		ObjectKey:        req.ObjectKey,
		OriginalFilename: req.OriginalFilename,
		MimeType:         req.MimeType,
		Size:             req.Size,
		Width:            req.Width,
		Height:           req.Height,
	}

	if err := db.DB.Create(&att).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建附件记录失败")
		return
	}

	att.URL = storage.SignResourceURL(att.ObjectKey)
	att.ThumbnailURL = ThumbnailURLFor(att.ObjectKey, 0)
	Success(c, att)
}

type CleanupUploadRequest struct {
	ObjectKeys []string `json:"object_keys" binding:"required"`
}

// CleanupUpload 清理未保存引用的孤立上传对象
func CleanupUpload(c *gin.Context) {
	var req CleanupUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少 object_keys")
		return
	}

	cleaned := 0
	for _, key := range req.ObjectKeys {
		if key != "" {
			var att models.Attachment
			// 仅清理三类归属都为空的真正孤立附件。
			// 只判 entry_id 会误删「心愿 / 足迹仍在引用、但散页已 purge」的共享照片。
			if err := db.DB.Where(
				"object_key = ? AND entry_id IS NULL AND wish_id IS NULL AND footprint_id IS NULL", key,
			).First(&att).Error; err == nil {
				_ = storage.DeleteObject(key)
				db.DB.Delete(&att)
				cleaned++
			}
		}
	}

	Success(c, gin.H{"cleaned_count": cleaned})
}

// GetStorageStatus 获取 R2 配置状态 (非管理员脱敏敏感桶名与节点)
func GetStorageStatus(c *gin.Context) {
	// 同一 URL 对访客/管理员返回不同字段，禁止任何共享缓存混用。
	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	c.Header("Cache-Control", "private, no-store")
	// BE-7：访客只需要两个标量。直接读缓存状态，绝不为了丢弃结果而遍历磁盘。
	if !middleware.IsAdmin(c) {
		Success(c, storage.GetStorageStatusBasic())
		return
	}
	Success(c, storage.GetStorageStatus())
}

// ==========================================
// 3. Transactions CRUD (记账接口)
// ==========================================

type TransactionRequest struct {
	Amount int64 `json:"amount" binding:"required"` // 分
	// MOB-P1-35：省略即默认 expense（与客户端默认值保持一致）
	Type          string `json:"type"` // income, expense
	Category      string `json:"category" binding:"required"`
	Title         string `json:"title"`
	Note          string `json:"note"`
	PaymentMethod string `json:"payment_method"`
	WishID        *uint  `json:"wish_id"`
	FootprintID   *uint  `json:"footprint_id"`
	CityCode      string `json:"city_code"`
	OccurredAt    string `json:"occurred_at"`
	// 分期付款核心字段 (RC-53)
	//
	// IsInstallment 用 **指针**：nil = 本次请求未携带该字段（更新时保持原值），
	// 显式 false = 明确降级为一次性支出。
	// 关键：原先是普通 bool，JSONEncoder 一定会把它写进请求体，
	// 于是客户端**不传**时也被解读成「显式取消分期」——
	// 而 Swift 的 `Bool` 不可选，任何一次「只改标题」的编辑都会把分期账目降级。
	IsInstallment *bool `json:"is_installment"`
	// ClearInstallment 显式声明「本次要取消分期」
	//
	// 关键：`is_installment: false` **不足以**表达「取消分期」。
	// 已发布的 mac 客户端（v1.8.1 ~ v2.0.1）的 `isInstallment` 是非可选 `Bool`，
	// JSONEncoder **必定**写出 `"is_installment": false` ——
	// 它压根不知道这个字段可以不传。
	// 若后端把 `false` 当成「取消分期」，那么老客户端**编辑一次账目**
	// 就会把分期降级成一次性支出：期数、月供、剩余期数、渠道全部清零，
	// 分期负债凭空消失、流动余额被整额冲减。
	//
	// 所以：**只有携带 clear_installment=true 时才真的降级**；
	// 单传 false 一律保持原值。
	ClearInstallment        bool   `json:"clear_installment"`
	InstallmentChannelCode  string `json:"installment_channel_code"`
	InstallmentTotalAmount  int64  `json:"installment_total_amount"`
	InstallmentMonths       int    `json:"installment_months"`
	InstallmentMonthlyPay   int64  `json:"installment_monthly_pay"`
	InstallmentRemainMonths int    `json:"installment_remain_months"`
	RepaymentDay            int    `json:"repayment_day"`
	// 双向联动：记账大件同步收录至随身设备库 (RC-53)
	LinkGearAsset bool   `json:"link_gear_asset"`
	GearCategory  string `json:"gear_category"`
	GearSpec      string `json:"gear_spec"`
	// ClearCity 显式清空城市归属（跨端统一契约 A）：优先级高于 city_code 字段本身。
	ClearCity bool `json:"clear_city"`
	// ClientRequestID 幂等创建标识（跨端统一契约 B）：同一 id 重复提交直接返回已有记录。
	ClientRequestID string `json:"client_request_id"`
}

// CreateTransaction 记一笔 (支持分期付款与随身设备库双向绑定 RC-53)
func CreateTransaction(c *gin.Context) {
	var req TransactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请填写金额、类型(支出/收入)与分类")
		return
	}
	// 关键：金额必须为正。
	// `binding:"required"` 对数值只拒绝 0，**负数放行** ——
	// 于是 `{"amount": -5000, "type":"expense"}` 会让支出总额**减少** ¥50，
	// 分类占比出现负比例，流动余额与净资产被反向虚增。
	if req.Amount <= 0 {
		Error(c, http.StatusBadRequest, "金额必须大于 0")
		return
	}

	occurred, err := ParseFlexTime(req.OccurredAt)
	if err != nil && strings.TrimSpace(req.OccurredAt) != "" {
		Error(c, http.StatusBadRequest, "发生时间格式无法识别")
		return
	}
	// 发现 18：城市只认显式 city_code（6 位国标或可反查字典），
	// 不再用标题/备注文本做模糊反查 ——「海南鸡饭」会命中青海海南州之类的误报。
	cleanCityCode, _, _ := ResolveCityCodeAndName(req.CityCode, "", "")

	// 契约 B：幂等创建。同一 client_request_id 的重复提交（重试/重放）直接返回已有记录；
	// 并发窗口由 client_request_id 唯一索引兜底（插入冲突后回读返回）。
	clientRequestID := strings.TrimSpace(req.ClientRequestID)
	if clientRequestID != "" {
		var existing models.Transaction
		if err := db.DB.Where("client_request_id = ?", clientRequestID).Limit(1).Find(&existing).Error; err == nil && existing.ID > 0 {
			Success(c, existing)
			return
		}
	}

	// MOB-P1-35：省略 type 时回落到默认 expense
	txType := strings.TrimSpace(req.Type)
	if txType == "" {
		txType = "expense"
	}

	tx := models.Transaction{
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
		tx.ClientRequestID = &clientRequestID
	}

	// 若选择了关联旅程 (FootprintID) 或带有城市代码，自动对齐城市并尝试归并到对应旅程
	autoLinkTransactionFootprint(&tx)

	if req.IsInstallment != nil && *req.IsInstallment {
		AssembleInstallmentTransaction(
			&tx,
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

	if err := db.DB.Create(&tx).Error; err != nil {
		// 并发重复提交：client_request_id 唯一索引兜底，回读已有记录按幂等语义返回
		//（含已被软删除的记录——该请求确实处理过，不该再记一笔）。
		if clientRequestID != "" {
			var existing models.Transaction
			if e2 := db.DB.Unscoped().Where("client_request_id = ?", clientRequestID).Limit(1).Find(&existing).Error; e2 == nil && existing.ID > 0 {
				Success(c, existing)
				return
			}
		}
		Error(c, http.StatusInternalServerError, "记账失败: "+err.Error())
		return
	}
	// 新账目可能挂在某趟旅程上：旅程花销统计随之变化，必须推进 footprints.updated_at，
	// 否则足迹列表的 ETag 不变，客户端会一直拿到 304 的旧花销。
	if tx.FootprintID != nil {
		touchFootprintTimestamps(db.DB, *tx.FootprintID)
	}
	// 关键：让 /stats 的 30 秒缓存失效。
	// 否则记完账立刻打开统计弹窗，弹窗显示的是**缓存里的旧数字**，
	// 而账本页（走 /transactions/summary，无缓存）显示新数字 ——
	// 同一个 App 里两个「本月支出」当场打架。
	InvalidateStatsCache()

	// 路径 B：记账大件同步收录至随身设备库 (RC-53)
	if req.LinkGearAsset {
		gearCategory := strings.TrimSpace(req.GearCategory)
		if gearCategory == "" {
			gearCategory = tx.Category
		}
		newGear := models.GearItem{
			ID:           fmt.Sprintf("gear_%s", generateRandomHex(8)),
			Name:         tx.Title,
			Spec:         strings.TrimSpace(req.GearSpec),
			Category:     gearCategory,
			Status:       "active",
			PriceCents:   tx.Amount,
			PurchaseDate: tx.OccurredAt.In(config.BeijingLocation).Format("2006-01-02"),
			Note:         tx.Note,
		}
		if err := mutateProfileGear(func(list []models.GearItem) ([]models.GearItem, error) {
			return append(list, newGear), nil
		}); err != nil {
			Error(c, http.StatusInternalServerError, "记账成功但收录设备资产失败: "+err.Error())
			return
		}
	}

	// 关联心愿单时同步闭环流转归册 (RG-14)
	if req.WishID != nil && *req.WishID > 0 {
		var wish models.Wish
		if err := db.DB.First(&wish, *req.WishID).Error; err == nil {
			if wish.Status == "wanting" {
				wish.Status = "fulfilled"
				occ := tx.OccurredAt
				wish.FulfilledAt = &occ
				wish.TransactionID = &tx.ID
				if wish.ActualCost == 0 {
					wish.ActualCost = tx.Amount
				}
				if err := db.DB.Save(&wish).Error; err != nil {
					Error(c, http.StatusInternalServerError, "记账成功但关联心愿状态更新失败: "+err.Error())
					return
				}
			}
		}
	}

	Success(c, tx)
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

	// MOB-P1-36：增量游标过滤统一走 applySyncSince（边界含等号）
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
		limit = 1000 // 钳制上限，防止超大 limit 整表载入内存 (BE-27)
	}
	if limit > 0 {
		query = query.Limit(limit)
	}
	if offset, err := strconv.Atoi(c.Query("offset")); err == nil && offset > 0 {
		query = query.Offset(offset)
	}

	// BE-2：聚合与 304 协商前置，命中时不再加载账目。
	agg, err := aggregateListQuery(query)
	if err != nil {
		Error(c, http.StatusInternalServerError, "获取记账列表失败")
		return
	}
	if negotiateListCache(c, listETag(c, agg, ""), agg.lastModified()) {
		return
	}

	var txs []models.Transaction
	if err := query.Find(&txs).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取记账列表失败")
		return
	}

	// 分期剩余期数按当前时间即时推算展示，不落库 (BE-24：GET 保持只读语义，避免扰动 ETag/304 与并发竞态)
	now := config.NowInBeijing()
	for i := range txs {
		if txs[i].IsInstallment {
			_, autoRemain := CalculateElapsedInstallmentMonths(txs[i].OccurredAt, txs[i].InstallmentMonths, txs[i].RepaymentDay, now)
			if txs[i].InstallmentRemainMonths > autoRemain {
				txs[i].InstallmentRemainMonths = autoRemain
			}
		}
	}

	// MOB-P1-36：服务端权威增量游标（客户端可直接回传为 ?since=）
	// 保持原有语义：取本页记录的最大变动时间（含软删除）。
	var pageMaxUpdated time.Time
	for _, tx := range txs {
		if tx.UpdatedAt.After(pageMaxUpdated) {
			pageMaxUpdated = tx.UpdatedAt
		}
		if tx.DeletedAt.Valid && tx.DeletedAt.Time.After(pageMaxUpdated) {
			pageMaxUpdated = tx.DeletedAt.Time
		}
	}
	setSyncCursor(c, pageMaxUpdated)

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
	db.DB.Select("type, category, amount").Where("occurred_at >= ? AND occurred_at < ?", start, end).Find(&txs)

	var totalExpense int64
	var totalIncome int64
	categoryBreakdown := make(map[string]int64)

	for _, tx := range txs {
		if tx.Type == "expense" {
			totalExpense += tx.Amount
			categoryBreakdown[tx.Category] += tx.Amount
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
	// 与原 binding:"required" 校验保持一致：amount 必填非 0，category 必填非空。
	// 改用手动校验是为了同时拿到原始请求体，支持「携带即写入」的部分更新判断（契约 C）。
	if req.Amount == 0 || strings.TrimSpace(req.Category) == "" {
		Error(c, http.StatusBadRequest, "请填写金额、类型(支出/收入)与分类")
		return
	}
	var rawKeys map[string]json.RawMessage
	_ = json.Unmarshal(rawBytes, &rawKeys)
	hasKey := func(k string) bool {
		_, ok := rawKeys[k]
		return ok
	}
	oldFootprintID := uint(0)
	if tx.FootprintID != nil {
		oldFootprintID = *tx.FootprintID
	}
	// 金额：**负数**必须被拒绝（会反向冲减所有汇总）；amount 必填非 0（上方已校验）。
	if req.Amount < 0 {
		Error(c, http.StatusBadRequest, "金额必须大于 0")
		return
	}

	tx.Amount = req.Amount
	// MOB-P1-xx：type 为空时**保持原值**，不能写成空串。
	//
	// 客户端用 encodeDefaults=false，等于默认值（expense）的字段会被整个省略 ⇒
	// PUT 请求体里根本没有 type 键 ⇒ req.Type 绑定成 ""。
	// 原先直接 `tx.Type = req.Type` ⇒ 支出账目编辑一次就从服务端消失了
	// （不命中任何 type='expense' 的过滤，支出统计与日合计随之失真），
	// 而本地还是乐观写回的 "expense" ⇒ 两端分叉，下一轮拉取才把空串同步回去。
	//
	// Update 的语义本就是「部分更新」：空串表示「这次没带这个字段」，
	// 于是保留原值（而不是回落成默认 expense —— 那样会把 income 账目错改成 expense）。
	if txType := strings.TrimSpace(req.Type); txType != "" {
		tx.Type = txType
	}
	tx.Category = strings.TrimSpace(req.Category)
	// 契约 C：可清空文本字段统一「请求携带该 key 即按其值写入（空串 = 清空），
	// 缺省 / null = 保持原值」。原先无条件覆盖，客户端省略 note 时会把备注清掉。
	if req.Title != "" || hasKey("title") {
		tx.Title = strings.TrimSpace(req.Title)
	}
	if req.Note != "" || hasKey("note") {
		tx.Note = strings.TrimSpace(req.Note)
	}
	if req.PaymentMethod != "" || hasKey("payment_method") {
		tx.PaymentMethod = strings.TrimSpace(req.PaymentMethod)
	}
	if req.WishID != nil {
		tx.WishID = req.WishID
	}
	if req.FootprintID != nil {
		if *req.FootprintID == 0 {
			tx.FootprintID = nil
		} else {
			tx.FootprintID = req.FootprintID
		}
	}
	// 契约 A：clear_city 显式清空城市归属，优先级高于 city_code 字段本身。
	clearCity := req.ClearCity
	if !clearCity && req.CityCode != "" {
		// 发现 18：只认显式 city_code，不再用标题/备注文本模糊反查
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

	autoLinkTransactionFootprint(&tx)
	if clearCity {
		// autoLink 会为已挂足迹的账目回填城市；clear_city 语义优先级最高，再次清空（契约 A）
		tx.CityCode = ""
	}

	if req.IsInstallment != nil && *req.IsInstallment {
		tx.IsInstallment = true
		if req.InstallmentChannelCode != "" {
			tx.InstallmentChannelCode = strings.TrimSpace(req.InstallmentChannelCode)
		}
		if req.InstallmentTotalAmount > 0 {
			tx.InstallmentTotalAmount = req.InstallmentTotalAmount
		} else if tx.InstallmentTotalAmount <= 0 {
			tx.InstallmentTotalAmount = tx.Amount
		}
		if req.InstallmentMonths > 0 {
			tx.InstallmentMonths = req.InstallmentMonths
		}
		if req.InstallmentMonthlyPay > 0 {
			tx.InstallmentMonthlyPay = req.InstallmentMonthlyPay
		} else if tx.InstallmentMonths > 0 {
			tx.InstallmentMonthlyPay = tx.InstallmentTotalAmount / int64(tx.InstallmentMonths)
		}
		if req.RepaymentDay >= 1 && req.RepaymentDay <= 31 {
			tx.RepaymentDay = req.RepaymentDay
		}
		_, autoRemain := CalculateElapsedInstallmentMonths(tx.OccurredAt, tx.InstallmentMonths, tx.RepaymentDay, time.Now())
		if req.InstallmentRemainMonths >= 0 && req.InstallmentRemainMonths <= autoRemain {
			tx.InstallmentRemainMonths = req.InstallmentRemainMonths
		} else {
			tx.InstallmentRemainMonths = autoRemain
		}
	} else if req.ClearInstallment {
		// 显式声明「取消分期」→ 真的降级并清空账期字段
		tx.IsInstallment = false
		tx.InstallmentChannelCode = ""
		tx.InstallmentTotalAmount = 0
		tx.InstallmentMonths = 0
		tx.InstallmentMonthlyPay = 0
		tx.InstallmentRemainMonths = 0
		tx.RepaymentDay = 0
	}
	// 注意：`is_installment: false` 但**没有** clear_installment 时，
	// 这里**故意什么都不做** —— 保持原分期状态。
	// 否则已发布的老客户端（它的 Bool 不可选，必定发 false）
	// 编辑一次账目就会毁掉分期负债。

	if err := db.DB.Save(&tx).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新账目失败")
		return
	}
	// 金额/挂载旅程任一变化都会影响旅程花销；旧旅程与新旅程都要推进时间戳。
	if tx.FootprintID != nil {
		touchFootprintTimestamps(db.DB, oldFootprintID, *tx.FootprintID)
	} else {
		touchFootprintTimestamps(db.DB, oldFootprintID)
	}
	// 同上：更新账目后统计缓存必须失效
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
		if err := db.DB.Unscoped().Delete(&tx).Error; err != nil {
			Error(c, http.StatusInternalServerError, "物理删除失败: "+err.Error())
			return
		}
		if tx.FootprintID != nil {
			touchFootprintTimestamps(db.DB, *tx.FootprintID)
		}
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	// 同上：删除账目后统计缓存必须失效
	defer InvalidateStatsCache()

	if err := db.DB.Delete(&tx).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	if tx.FootprintID != nil {
		touchFootprintTimestamps(db.DB, *tx.FootprintID)
	}
	Success(c, gin.H{"id": id, "deleted": true})
}

// ==========================================
// 4. Projects CRUD (项目与造物接口)
// ==========================================

type ProjectRequest struct {
	Title       string `json:"title" binding:"required"`
	Subtitle    string `json:"subtitle"`
	Description string `json:"description"`
	Story       string `json:"story"`
	Category    string `json:"category"`
	GithubURL   string `json:"github_url"`
	Status      string `json:"status"`
	Order       int    `json:"order"`
}

func CreateProject(c *gin.Context) {
	var req ProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "项目标题必填且格式有效")
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		Error(c, http.StatusBadRequest, "项目标题必填")
		return
	}

	// MOB-P1-35：省略 status 时回落到默认「进行中」
	projectStatus := strings.TrimSpace(req.Status)
	if projectStatus == "" {
		projectStatus = "进行中"
	}

	p := models.Project{
		Title:       title,
		Subtitle:    strings.TrimSpace(req.Subtitle),
		Description: strings.TrimSpace(req.Description),
		Story:       strings.TrimSpace(req.Story),
		Category:    strings.TrimSpace(req.Category),
		GithubURL:   strings.TrimSpace(req.GithubURL),
		Status:      projectStatus,
		Order:       req.Order,
	}
	if p.Status == "" {
		p.Status = "Active"
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&p).Error; err != nil {
			return err
		}

		// 伴生创建机制：在【造物】立项时自动伴生创建一条对应时间的一级记录写入主时间流 (保证事务原子性)
		_, err := createProjectCompanionEntry(tx, p)
		return err
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "创建项目及伴生记录失败: "+err.Error())
		return
	}
	InvalidateStatsCache()

	Success(c, p)
}

// GetProjectsV1 获取项目列表 (支持增量游标与 ETag 缓存协商 BE-05)
func GetProjectsV1(c *gin.Context) {
	sinceStr := c.Query("since")
	includeDeleted := c.Query("include_deleted") == "true" || sinceStr != ""
	if !validateSyncSince(c, sinceStr) || !allowDeletedScope(c, includeDeleted) {
		return
	}

	dbScope := db.DB
	if includeDeleted {
		dbScope = db.DB.Unscoped()
	}

	query := dbScope.Model(&models.Project{}).Order("`order` ASC, id DESC")

	// MOB-P1-36：增量游标过滤统一走 applySyncSince（边界含等号）
	query = applySyncSince(query, &models.Project{}, sinceStr)

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "500"))
	if limit > 1000 {
		limit = 1000 // 钳制上限，防止超大 limit 整表载入内存 (BE-27)
	}
	if limit > 0 {
		query = query.Limit(limit)
	}

	// BE-2：聚合与 304 协商前置，命中时不再加载项目列表。
	agg, err := aggregateListQuery(query)
	if err != nil {
		Error(c, http.StatusInternalServerError, "获取项目列表失败")
		return
	}
	if negotiateListCache(c, listETag(c, agg, ""), agg.lastModified()) {
		return
	}

	var projects []models.Project
	if err := query.Find(&projects).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取项目列表失败")
		return
	}

	// MOB-P1-36：服务端权威增量游标（保持本页语义）
	var pageMaxUpdated time.Time
	for _, p := range projects {
		if p.UpdatedAt.After(pageMaxUpdated) {
			pageMaxUpdated = p.UpdatedAt
		}
		if p.DeletedAt.Valid && p.DeletedAt.Time.After(pageMaxUpdated) {
			pageMaxUpdated = p.DeletedAt.Time
		}
	}
	setSyncCursor(c, pageMaxUpdated)

	Success(c, projects)
}

func GetProjectByID(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	var p models.Project
	if err := db.DB.First(&p, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "项目不存在")
		return
	}

	// 同时拉取关联该项目的 Life Entries
	var entries []models.LifeEntry
	db.DB.Where("project_id = ?", p.ID).Order("occurred_at DESC").Find(&entries)
	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	populateEntryURLsBatch(entries)
	if !middleware.IsAdmin(c) {
		redactEmbeddedWishFinancesBatch(entries)
	}

	Success(c, gin.H{
		"project": p,
		"entries": entries,
	})
}

type ProjectUpdateRequest struct {
	Title       *string `json:"title"`
	Subtitle    *string `json:"subtitle"`
	Description *string `json:"description"`
	Story       *string `json:"story"`
	Category    *string `json:"category"`
	GithubURL   *string `json:"github_url"`
	Status      *string `json:"status"`
	Order       *int    `json:"order"`
}

func UpdateProject(c *gin.Context) {
	id := c.Param("id")
	var p models.Project
	if err := db.DB.First(&p, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "项目不存在")
		return
	}

	var req ProjectUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	oldStory := p.Story
	oldStatus := p.Status
	oldTitle := p.Title

	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" {
			Error(c, http.StatusBadRequest, "项目标题不能为空")
			return
		}
		p.Title = title
	}
	if req.Subtitle != nil {
		p.Subtitle = strings.TrimSpace(*req.Subtitle)
	}
	if req.Description != nil {
		p.Description = strings.TrimSpace(*req.Description)
	}
	if req.Story != nil {
		p.Story = strings.TrimSpace(*req.Story)
	}
	if req.Category != nil {
		p.Category = strings.TrimSpace(*req.Category)
	}
	if req.GithubURL != nil {
		p.GithubURL = strings.TrimSpace(*req.GithubURL)
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != "" {
			p.Status = status
		}
	}
	if req.Order != nil {
		p.Order = *req.Order
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		// 关键：先把「立项」那条伴生散页同步到最新的标题 / 正文，
		// 否则改了项目名，时间流里仍是旧标题。
		if err := syncProjectCompanionEntry(tx, p, oldTitle); err != nil {
			return err
		}

		if err := tx.Save(&p).Error; err != nil {
			return err
		}

		// 伴生创建机制：当造物项目更新进展手记 (story) 或状态变迁 (status) 时，自动伴生创建一条一级记录写入主时间流
		storyChanged := req.Story != nil && p.Story != "" && p.Story != oldStory
		statusChanged := req.Status != nil && p.Status != "" && p.Status != oldStatus
		if storyChanged || statusChanged {
			entryTitle := fmt.Sprintf("%s · 造物手记", p.Title)
			if statusChanged && !storyChanged {
				entryTitle = fmt.Sprintf("%s · 状态演进 (%s)", p.Title, p.Status)
			}
			entryContent := p.Story
			if !storyChanged || entryContent == "" {
				if p.Description != "" {
					entryContent = p.Description
				} else {
					entryContent = fmt.Sprintf("造物项目「%s」进展更新，当前状态：%s。", p.Title, p.Status)
				}
			}
			progressEntry := models.LifeEntry{
				Type:       "project",
				Title:      entryTitle,
				Content:    entryContent,
				OccurredAt: time.Now().In(config.BeijingLocation),
				ProjectID:  &p.ID,
			}
			if err := tx.Create(&progressEntry).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "更新项目及伴生进展失败: "+err.Error())
		return
	}
	InvalidateStatsCache()

	Success(c, p)
}

// DeleteProject 删除项目 (默认软删除，?purge=true 为物理清理 BE-05)
func DeleteProject(c *gin.Context) {
	id := c.Param("id")
	purge := c.Query("purge") == "true"

	var p models.Project
	if err := db.DB.First(&p, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "项目不存在或已被删除")
		return
	}

	if purge {
		err := db.DB.Transaction(func(tx *gorm.DB) error {
			// 物理删除项目时，一并物理删除它在时间流里的伴生散页
			tx.Unscoped().Where("project_id = ?", p.ID).Delete(&models.LifeEntry{})
			return tx.Unscoped().Delete(&p).Error
		})
		if err != nil {
			Error(c, http.StatusInternalServerError, "物理删除失败: "+err.Error())
			return
		}
		InvalidateStatsCache()
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		// 关键：软删除项目时一并软删除伴生散页，
		// 否则时间流里会留下一条再也点不回项目的孤儿记录。
		if err := deleteProjectCompanionEntries(tx, p.ID); err != nil {
			return err
		}
		return tx.Delete(&p).Error
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	InvalidateStatsCache()
	Success(c, gin.H{"id": id, "deleted": true})
}

// ==========================================
// 5. Global Stats (生活档案统计概览)
// ==========================================

type statsSnapshot struct {
	TotalEntries  int64
	TotalPhotos   int64
	TotalProjects int64
	MonthExpense  int64
	CurrentYear   int
	CurrentMonth  int
}

var (
	statsCacheMutex sync.RWMutex
	cachedStats     *statsSnapshot
	cachedStatsTime time.Time
)

// InvalidateStatsCache 主动失效全局统计缓存
func InvalidateStatsCache() {
	statsCacheMutex.Lock()
	cachedStats = nil
	cachedStatsTime = time.Time{}
	statsCacheMutex.Unlock()
}

func GetGlobalStats(c *gin.Context) {
	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	isAdmin := middleware.IsAdmin(c)

	now := time.Now()
	statsCacheMutex.RLock()
	if cachedStats != nil && now.Sub(cachedStatsTime) < 30*time.Second {
		snap := *cachedStats
		statsCacheMutex.RUnlock()
		expense := snap.MonthExpense
		if !isAdmin {
			expense = 0
		}
		Success(c, gin.H{
			"total_entries":  snap.TotalEntries,
			"total_photos":   snap.TotalPhotos,
			"total_projects": snap.TotalProjects,
			"month_expense":  expense,
			"current_year":   snap.CurrentYear,
			"current_month":  snap.CurrentMonth,
		})
		return
	}
	statsCacheMutex.RUnlock()

	var totalEntries int64
	db.DB.Model(&models.LifeEntry{}).Count(&totalEntries)

	// 照片数：按 MIME 前缀统计图片附件（RC-128 四维收敛后不再存在 type='photo' 的条目，死分支已移除）
	var totalPhotos int64
	db.DB.Model(&models.Attachment{}).Where("mime_type LIKE ?", "image/%").Count(&totalPhotos)

	var totalProjects int64
	db.DB.Model(&models.Project{}).Count(&totalProjects)

	// 当月支出（按北京时间月份边界计算 RC-134）
	nowBeijing := now.In(config.BeijingLocation)
	start := time.Date(nowBeijing.Year(), nowBeijing.Month(), 1, 0, 0, 0, 0, config.BeijingLocation)
	end := start.AddDate(0, 1, 0)

	var monthExpense int64
	type Result struct {
		Total int64
	}
	var res Result
	db.DB.Model(&models.Transaction{}).
		Select("COALESCE(SUM(amount), 0) as total").
		Where("type = 'expense' AND occurred_at >= ? AND occurred_at < ?", start, end).
		Scan(&res)
	monthExpense = res.Total

	snap := statsSnapshot{
		TotalEntries:  totalEntries,
		TotalPhotos:   totalPhotos,
		TotalProjects: totalProjects,
		MonthExpense:  monthExpense,
		CurrentYear:   nowBeijing.Year(),
		CurrentMonth:  int(nowBeijing.Month()),
	}

	statsCacheMutex.Lock()
	cachedStats = &snap
	cachedStatsTime = now
	statsCacheMutex.Unlock()

	visibleExpense := monthExpense
	if !isAdmin {
		visibleExpense = 0
	}

	Success(c, gin.H{
		"total_entries":  snap.TotalEntries,
		"total_photos":   snap.TotalPhotos,
		"total_projects": snap.TotalProjects,
		"month_expense":  visibleExpense,
		"current_year":   snap.CurrentYear,
		"current_month":  snap.CurrentMonth,
	})
}

// ==========================================
// 6. Global Site Configuration & Health Routes (站点全局配置与健康探针 RC-90)

func GetConfigV1(c *gin.Context) {
	var config models.SiteConfig
	if err := db.DB.Limit(1).Find(&config).Error; err != nil || config.ID == 0 {
		config = models.SiteConfig{
			SiteName:        "散页 · 个人生活志",
			SiteSubtitle:    "日常 · 资产 · 心愿 · 行迹 · 造物",
			CuratorName:     "",
			DefaultLocation: "",
		}
	}
	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	if !middleware.IsAdmin(c) {
		config.InitialLiquidBalance = 0
	}
	Success(c, config)
}

func UpdateConfig(c *gin.Context) {
	var config models.SiteConfig
	db.DB.FirstOrCreate(&config)

	var req models.SiteConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	if req.SiteName != "" {
		config.SiteName = strings.TrimSpace(req.SiteName)
	}
	if req.SiteSubtitle != "" {
		config.SiteSubtitle = strings.TrimSpace(req.SiteSubtitle)
	}
	if req.CuratorName != "" {
		config.CuratorName = strings.TrimSpace(req.CuratorName)
	}
	if req.DefaultLocation != "" {
		config.DefaultLocation = strings.TrimSpace(req.DefaultLocation)
	}
	if req.SiteDesc != "" {
		config.SiteDesc = strings.TrimSpace(req.SiteDesc)
	}

	if err := db.DB.Save(&config).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新站点配置失败: "+err.Error())
		return
	}

	Success(c, config)
}

func HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":     "operational",
		"service":    "Loose-Leaf Life Archive Core",
		"version":    config.Version,
		"runtime":    runtime.Version(),
		"storage_r2": storage.IsConfigured(),
		"time":       time.Now().Unix(),
		"message":    fmt.Sprintf("Archive engine %s running quietly on %s", config.Version, runtime.GOARCH),
	})
}

// GlobalSearch 全站跨维度聚合搜索 (GET /api/v1/search?q=...&limit=...)
func GlobalSearch(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		Success(c, gin.H{
			"query":      "",
			"entries":    []models.LifeEntry{},
			"footprints": []models.Footprint{},
			"wishes":     []models.Wish{},
			"projects":   []models.Project{},
		})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	if limit < 1 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}

	likePat := "%" + q + "%"

	var entries []models.LifeEntry
	db.DB.Where("title LIKE ? OR content LIKE ? OR location LIKE ? OR city_name LIKE ?", likePat, likePat, likePat, likePat).
		Order("occurred_at DESC, id DESC").
		Limit(limit).
		Find(&entries)
	if entries == nil {
		entries = []models.LifeEntry{}
	}
	populateEntryURLsBatch(entries)
	if !middleware.IsAdmin(c) {
		redactEmbeddedWishFinancesBatch(entries)
	}

	var footprints []models.Footprint
	db.DB.Where("city_name LIKE ? OR title LIKE ? OR note LIKE ?", likePat, likePat, likePat).
		Order("visited_at DESC, id DESC").
		Limit(limit).
		Find(&footprints)
	if footprints == nil {
		footprints = []models.Footprint{}
	}
	populateFootprintURLsBatch(footprints)

	var wishes []models.Wish
	db.DB.Where("title LIKE ? OR note LIKE ? OR spec LIKE ? OR city_name LIKE ?", likePat, likePat, likePat, likePat).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&wishes)
	if wishes == nil {
		wishes = []models.Wish{}
	}
	populateWishURLsBatch(wishes)
	isAdmin := middleware.IsAdmin(c)
	if !isAdmin {
		for i := range wishes {
			wishes[i].ActualCost = 0
			wishes[i].TransactionID = nil
			wishes[i].GearID = nil
		}
	}

	var projects []models.Project
	db.DB.Where("title LIKE ? OR subtitle LIKE ? OR description LIKE ? OR story LIKE ? OR category LIKE ?", likePat, likePat, likePat, likePat, likePat).
		Order("`order` ASC, id DESC").
		Limit(limit).
		Find(&projects)
	if projects == nil {
		projects = []models.Project{}
	}

	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key, Cookie")
	Success(c, gin.H{
		"query":      q,
		"entries":    entries,
		"footprints": footprints,
		"wishes":     wishes,
		"projects":   projects,
	})
}
