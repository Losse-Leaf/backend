package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
)

// GetAnnualAlmanacSummary 年度生活年鉴聚合计算接口 (五幕叙事：岁月卷首 / 山川行迹 / 心愿圆满 / 浮光掠影 / 柴米油盐)
func GetAnnualAlmanacSummary(c *gin.Context) {
	nowBeijing := config.NowInBeijing()
	yearStr := c.DefaultQuery("year", fmt.Sprintf("%04d", nowBeijing.Year()))
	year, err := strconv.Atoi(yearStr)
	if err != nil || year < 1900 || year > 3000 {
		year = nowBeijing.Year()
	}

	startOfYear := time.Date(year, 1, 1, 0, 0, 0, 0, config.BeijingLocation)
	endOfYear := startOfYear.AddDate(1, 0, 0)

	// 1. 第一幕：岁月卷首数据 (LifeEntries 聚合)
	var entries []models.LifeEntry
	db.DB.Where("occurred_at >= ? AND occurred_at < ?", startOfYear, endOfYear).
		Order("occurred_at ASC").
		Find(&entries)

	populateEntryURLsBatch(entries)

	distinctDays := make(map[string]bool)
	totalWords := 0
	beijing := config.BeijingLocation
	for _, e := range entries {
		dStr := e.OccurredAt.In(beijing).Format("2006-01-02")
		distinctDays[dStr] = true
		totalWords += len([]rune(e.Content))
	}

	firstDateStr := ""
	lastDateStr := ""
	if len(entries) > 0 {
		firstDateStr = entries[0].OccurredAt.In(beijing).Format("2006-01-02")
		lastDateStr = entries[len(entries)-1].OccurredAt.In(beijing).Format("2006-01-02")
	}

	// 2. 第二幕：山川行迹 (Footprints 聚合)
	var footprints []models.Footprint
	db.DB.Where("visited_at >= ? AND visited_at < ?", startOfYear, endOfYear).
		Order("visited_at ASC").
		Find(&footprints)

	populateFootprintURLsBatch(footprints)

	yearCityMap := make(map[string]string)
	for _, f := range footprints {
		yearCityMap[f.CityCode] = f.CityName
	}

	type citySummary struct {
		CityCode string `json:"city_code"`
		CityName string `json:"city_name"`
	}
	citiesVisited := make([]citySummary, 0, len(yearCityMap))
	for code, name := range yearCityMap {
		citiesVisited = append(citiesVisited, citySummary{CityCode: code, CityName: name})
	}

	// 3. 第三幕：心愿圆满 (Wishes 聚合)
	var fulfilledWishes []models.Wish
	db.DB.Where("fulfilled_at >= ? AND fulfilled_at < ?", startOfYear, endOfYear).
		Where("status = ?", "fulfilled").
		Order("fulfilled_at ASC").
		Find(&fulfilledWishes)

	populateWishURLsBatch(fulfilledWishes)

	var wantingWishCount int64
	db.DB.Model(&models.Wish{}).Where("status = ?", "wanting").Count(&wantingWishCount)

	var totalWishBudget, totalWishActual int64
	for _, w := range fulfilledWishes {
		totalWishBudget += w.Budget
		totalWishActual += w.ActualCost
	}

	// 4. 第四幕：浮光掠影 (四季高光与菲林相片)
	var springHighlight, summerHighlight, autumnHighlight, winterHighlight *models.LifeEntry
	filmGallery := make([]models.Attachment, 0)

	for i, e := range entries {
		m := e.OccurredAt.In(beijing).Month()
		switch {
		case m >= 3 && m <= 5:
			if springHighlight == nil || len(e.Attachments) > len(springHighlight.Attachments) {
				springHighlight = &entries[i]
			}
		case m >= 6 && m <= 8:
			if summerHighlight == nil || len(e.Attachments) > len(summerHighlight.Attachments) {
				summerHighlight = &entries[i]
			}
		case m >= 9 && m <= 11:
			if autumnHighlight == nil || len(e.Attachments) > len(autumnHighlight.Attachments) {
				autumnHighlight = &entries[i]
			}
		default:
			if winterHighlight == nil || len(e.Attachments) > len(winterHighlight.Attachments) {
				winterHighlight = &entries[i]
			}
		}

		for _, att := range e.Attachments {
			filmGallery = append(filmGallery, att)
		}
	}

	// 截取最多 24 张精选照片作为长胶卷（优先展示最新的精选胶片）
	if len(filmGallery) > 24 {
		filmGallery = filmGallery[len(filmGallery)-24:]
	}

	if footprints == nil {
		footprints = make([]models.Footprint, 0)
	}
	if fulfilledWishes == nil {
		fulfilledWishes = make([]models.Wish, 0)
	}

	// 5. 第五幕：柴米油盐 (Transactions 聚合：仅主理人鉴权请求查询聚合，访客模式零查询脱敏 BE-14)
	isAdmin := middleware.IsAdmin(c)
	var txs []models.Transaction
	if isAdmin {
		db.DB.Where("occurred_at >= ? AND occurred_at < ?", startOfYear, endOfYear).Find(&txs)
	}

	var totalIncome, totalExpense, wishExpenses int64
	categoryBreakdown := make(map[string]int64)

	for _, tx := range txs {
		if tx.Type == "expense" {
			totalExpense += tx.Amount
			categoryBreakdown[tx.Category] += tx.Amount
			if tx.WishID != nil || tx.Category == "好物购置" || tx.Category == "旅行度假" {
				wishExpenses += tx.Amount
			}
		} else if tx.Type == "income" {
			totalIncome += tx.Amount
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

	wishRatio := 0.0
	if totalExpense > 0 {
		wishRatio = float64(wishExpenses) / float64(totalExpense)
	}

	txCount := len(txs)
	finSpectrum := gin.H{
		"total_income":       totalIncome,
		"total_expense":      totalExpense,
		"balance":            totalIncome - totalExpense,
		"wish_expenses":      wishExpenses,
		"wish_spending_ratio": wishRatio * 100.0, // 百分比，唯一权威键 (BE-38 去除同义双键)
		"categories":          categories,
	}

	// 访客模式财务隐私严格脱敏 (BE-14)
	if !isAdmin {
		txCount = 0
		totalWishActual = 0
		for i := range fulfilledWishes {
			fulfilledWishes[i].ActualCost = 0
			fulfilledWishes[i].TransactionID = nil
			fulfilledWishes[i].GearID = nil
		}
		finSpectrum = gin.H{
			"total_income":        0,
			"total_expense":       0,
			"balance":             0,
			"wish_expenses":       0,
			"wish_spending_ratio": 0.0,
			"categories":          []catItem{},
		}
	}

	// 组装年鉴信封 (附带权限角色后缀防止跨权限 304 缓存污染 BE-17，同时纳入内容更新指纹)
	role := "guest"
	if isAdmin {
		role = "admin"
	}
	var maxUpdated time.Time
	for _, e := range entries {
		if e.UpdatedAt.After(maxUpdated) {
			maxUpdated = e.UpdatedAt
		}
	}
	for _, f := range footprints {
		if f.UpdatedAt.After(maxUpdated) {
			maxUpdated = f.UpdatedAt
		}
	}
	for _, w := range fulfilledWishes {
		if w.UpdatedAt.After(maxUpdated) {
			maxUpdated = w.UpdatedAt
		}
	}
	if isAdmin {
		for _, tx := range txs {
			if tx.UpdatedAt.After(maxUpdated) {
				maxUpdated = tx.UpdatedAt
			}
		}
	}

	etag := fmt.Sprintf(`W/"almanac-%d-%d-%d-%d-%s"`, year, len(entries), len(txs), maxUpdated.UnixMilli(), role)
	c.Header("ETag", etag)
	c.Header("Vary", "Authorization, X-Device-Token, X-Admin-Key")

	if match := c.GetHeader("If-None-Match"); match != "" && match == etag {
		c.Status(http.StatusNotModified)
		return
	}

	atlasPayload := gin.H{
		"total_cities": len(citiesVisited),
		"cities":       citiesVisited,
		"footprints":   footprints,
	}
	wishesPayload := gin.H{
		"wishes":          fulfilledWishes,
		"fulfilled_count": len(fulfilledWishes),
		"wanting_count":   wantingWishCount,
		"total_budget":    totalWishBudget,
		"total_actual":    totalWishActual,
	}
	highlightsPayload := gin.H{
		"spring": springHighlight,
		"summer": summerHighlight,
		"autumn": autumnHighlight,
		"winter": winterHighlight,
		"photos": filmGallery,
	}

	Success(c, gin.H{
		"year": year,
		"prologue": gin.H{
			"total_entries":    len(entries),
			"total_words":      totalWords,
			"active_days":      len(distinctDays),
			"first_date":       firstDateStr,
			"last_date":        lastDateStr,
			"wishes_fulfilled": len(fulfilledWishes),
			"cities_visited":   len(citiesVisited),
			"tx_count":         txCount,
		},
		"atlas":      atlasPayload,
		"wishes":     wishesPayload,
		"highlights": highlightsPayload,
		"finances":   finSpectrum,
	})
}
