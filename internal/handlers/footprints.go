package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"
)

// populateFootprintURLs 将足迹相片签名转化为可访问链接
func populateFootprintURLs(f *models.Footprint) {
	for i := range f.Attachments {
		f.Attachments[i].URL = storage.SignResourceURL(f.Attachments[i].ObjectKey)
	}
}

// CreateFootprintRequest 录入足迹请求
type CreateFootprintRequest struct {
	CityCode      string  `json:"city_code" binding:"required"`
	CityName      string  `json:"city_name" binding:"required"`
	VisitedAt     string  `json:"visited_at" binding:"required"`
	Note          string  `json:"note"`
	LifeEntryID   *uint   `json:"life_entry_id"`
	WishID        *uint   `json:"wish_id"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	AttachmentIDs []uint  `json:"attachment_ids"`
}

// UpdateFootprintRequest 修改足迹请求
type UpdateFootprintRequest struct {
	CityCode      string  `json:"city_code"`
	CityName      string  `json:"city_name"`
	VisitedAt     string  `json:"visited_at"`
	Note          string  `json:"note"`
	LifeEntryID   *uint   `json:"life_entry_id"`
	WishID        *uint   `json:"wish_id"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	AttachmentIDs []uint  `json:"attachment_ids"`
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

	query := dbScope.Model(&models.Footprint{}).Preload("Attachments").Order("visited_at DESC, id DESC")

	if sinceStr != "" {
		if sinceTime, err := ParseFlexTime(sinceStr); err == nil {
			query = query.Where("updated_at > ? OR deleted_at > ?", sinceTime, sinceTime)
		}
	}

	if cityCode != "" {
		query = query.Where("city_code = ?", cityCode)
	}

	if yearStr != "" {
		if y, err := strconv.Atoi(yearStr); err == nil && y > 1900 && y < 3000 {
			start := time.Date(y, 1, 1, 0, 0, 0, 0, time.Local)
			end := start.AddDate(1, 0, 0)
			query = query.Where("visited_at >= ? AND visited_at < ?", start, end)
		}
	}

	var footprints []models.Footprint
	if err := query.Find(&footprints).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取足迹列表失败")
		return
	}

	for i := range footprints {
		populateFootprintURLs(&footprints[i])
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
	if err := db.DB.Preload("Attachments").First(&f, id).Error; err != nil {
		Error(c, http.StatusNotFound, "足迹记录不存在")
		return
	}
	populateFootprintURLs(&f)
	Success(c, f)
}

// CreateFootprint 录入足迹 (需管理员权限)
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

	footprint := models.Footprint{
		CityCode:    strings.TrimSpace(req.CityCode),
		CityName:    strings.TrimSpace(req.CityName),
		VisitedAt:   visited,
		Note:        strings.TrimSpace(req.Note),
		LifeEntryID: req.LifeEntryID,
		WishID:      req.WishID,
		Latitude:    req.Latitude,
		Longitude:   req.Longitude,
	}

	if err := db.DB.Create(&footprint).Error; err != nil {
		Error(c, http.StatusInternalServerError, "保存足迹记录失败: "+err.Error())
		return
	}

	if len(req.AttachmentIDs) > 0 {
		db.DB.Model(&models.Attachment{}).
			Where("id IN ?", req.AttachmentIDs).
			Update("footprint_id", footprint.ID)
	}

	db.DB.Preload("Attachments").First(&footprint, footprint.ID)
	populateFootprintURLs(&footprint)

	Success(c, footprint)
}

// UpdateFootprint 修改足迹
func UpdateFootprint(c *gin.Context) {
	id := c.Param("id")
	var footprint models.Footprint
	if err := db.DB.Preload("Attachments").First(&footprint, id).Error; err != nil {
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
	if req.VisitedAt != "" {
		if t, err := ParseFlexTime(req.VisitedAt); err == nil {
			footprint.VisitedAt = t
		}
	}
	if req.Note != "" {
		footprint.Note = strings.TrimSpace(req.Note)
	}
	footprint.LifeEntryID = req.LifeEntryID
	footprint.WishID = req.WishID
	if req.Latitude != 0 {
		footprint.Latitude = req.Latitude
	}
	if req.Longitude != 0 {
		footprint.Longitude = req.Longitude
	}

	if err := db.DB.Save(&footprint).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新足迹失败")
		return
	}

	if req.AttachmentIDs != nil {
		db.DB.Model(&models.Attachment{}).
			Where("footprint_id = ? AND id NOT IN ?", footprint.ID, req.AttachmentIDs).
			Update("footprint_id", nil)

		if len(req.AttachmentIDs) > 0 {
			db.DB.Model(&models.Attachment{}).
				Where("id IN ?", req.AttachmentIDs).
				Update("footprint_id", footprint.ID)
		}
	}

	db.DB.Preload("Attachments").First(&footprint, footprint.ID)
	populateFootprintURLs(&footprint)

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
		if err := db.DB.Unscoped().Delete(&f).Error; err != nil {
			Error(c, http.StatusInternalServerError, "物理删除失败: "+err.Error())
			return
		}
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	if err := db.DB.Delete(&f).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
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
	var footprints []models.Footprint
	db.DB.Order("visited_at ASC").Find(&footprints)

	type cityAgg struct {
		cityCode  string
		cityName  string
		count     int
		firstDate time.Time
		lastDate  time.Time
	}

	cityMap := make(map[string]*cityAgg)
	for _, f := range footprints {
		agg, ok := cityMap[f.CityCode]
		if !ok {
			cityMap[f.CityCode] = &cityAgg{
				cityCode:  f.CityCode,
				cityName:  f.CityName,
				count:     1,
				firstDate: f.VisitedAt,
				lastDate:  f.VisitedAt,
			}
		} else {
			agg.count++
			if f.VisitedAt.Before(agg.firstDate) {
				agg.firstDate = f.VisitedAt
			}
			if f.VisitedAt.After(agg.lastDate) {
				agg.lastDate = f.VisitedAt
			}
		}
	}

	cities := make([]AtlasCitySummary, 0, len(cityMap))
	for _, agg := range cityMap {
		cities = append(cities, AtlasCitySummary{
			CityCode:        agg.cityCode,
			CityName:        agg.cityName,
			VisitCount:      agg.count,
			FirstVisitedAt:  agg.firstDate.Format("2006-01-02"),
			LatestVisitedAt: agg.lastDate.Format("2006-01-02"),
		})
	}

	Success(c, gin.H{
		"total_footprints": len(footprints),
		"total_cities":     len(cities),
		"cities":           cities,
	})
}

// GetCityMemoryDrawer 城市回忆抽屉聚合接口 (履迹简志 + 菲林画廊 + 散页随笔 + 城市账单光谱)
func GetCityMemoryDrawer(c *gin.Context) {
	cityCode := c.Param("city_code")
	if cityCode == "" {
		Error(c, http.StatusBadRequest, "城市代码必填")
		return
	}

	// 1. 该城市的 Footprints 列表与相片
	var footprints []models.Footprint
	db.DB.Where("city_code = ?", cityCode).Preload("Attachments").Order("visited_at DESC").Find(&footprints)

	cityName := ""
	if len(footprints) > 0 {
		cityName = footprints[0].CityName
	}

	var firstVisit, latestVisit time.Time
	for i, f := range footprints {
		populateFootprintURLs(&footprints[i])
		if firstVisit.IsZero() || f.VisitedAt.Before(firstVisit) {
			firstVisit = f.VisitedAt
		}
		if latestVisit.IsZero() || f.VisitedAt.After(latestVisit) {
			latestVisit = f.VisitedAt
		}
	}

	firstVisitStr := ""
	latestVisitStr := ""
	if !firstVisit.IsZero() {
		firstVisitStr = firstVisit.Format("2006-01-02")
	}
	if !latestVisit.IsZero() {
		latestVisitStr = latestVisit.Format("2006-01-02")
	}

	// 2. 关联该城市的 LifeEntries 随笔
	var entries []models.LifeEntry
	dbQuery := db.DB.Preload("Attachments").Order("occurred_at DESC")
	if cityName != "" {
		dbQuery = dbQuery.Where("city_code = ? OR location LIKE ?", cityCode, "%"+cityName+"%")
	} else {
		dbQuery = dbQuery.Where("city_code = ?", cityCode)
	}
	dbQuery.Limit(50).Find(&entries)

	for i := range entries {
		populateEntryURLs(&entries[i])
	}

	// 3. 关联该城市的消费账单光谱
	var txs []models.Transaction
	txQuery := db.DB.Where("city_code = ?", cityCode)
	if cityName != "" {
		txQuery = db.DB.Where("city_code = ? OR note LIKE ? OR title LIKE ?", cityCode, "%"+cityName+"%", "%"+cityName+"%")
	}
	txQuery.Find(&txs)

	var totalSpent int64
	categoryBreakdown := make(map[string]int64)
	for _, tx := range txs {
		if tx.Type == "expense" {
			totalSpent += tx.Amount
			categoryBreakdown[tx.Category] += tx.Amount
		}
	}

	type catItem struct {
		Name   string `json:"name"`
		Amount int64  `json:"amount"`
	}
	categories := make([]catItem, 0, len(categoryBreakdown))
	for name, amt := range categoryBreakdown {
		categories = append(categories, catItem{Name: name, Amount: amt})
	}

	var ledgerSummary any
	if middleware.IsAdmin(c) {
		ledgerSummary = gin.H{
			"total_spent":       totalSpent,
			"transaction_count": len(txs),
			"categories":        categories,
		}
	} else {
		// 访客模式财务隐私严格脱敏 (BE-14)
		ledgerSummary = gin.H{
			"total_spent":       0,
			"transaction_count": 0,
			"categories":        []catItem{},
		}
	}

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
