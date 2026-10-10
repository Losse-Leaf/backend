package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// GetAnnualAlmanacSummary 年度生活年鉴聚合计算接口 (五幕叙事：岁月卷首 / 山川行迹 / 心愿圆满 / 浮光掠影 / 柴米油盐)
func GetAnnualAlmanacSummary(c *gin.Context) {
	yearStr := c.DefaultQuery("year", fmt.Sprintf("%04d", time.Now().Year()))
	year, err := strconv.Atoi(yearStr)
	if err != nil || year < 1900 || year > 3000 {
		year = time.Now().Year()
	}

	startOfYear := time.Date(year, 1, 1, 0, 0, 0, 0, time.Local)
	endOfYear := startOfYear.AddDate(1, 0, 0)

	// 1. 第一幕：岁月卷首数据 (LifeEntries 聚合)
	var entries []models.LifeEntry
	db.DB.Where("occurred_at >= ? AND occurred_at < ?", startOfYear, endOfYear).
		Preload("Attachments").
		Order("occurred_at ASC").
		Find(&entries)

	for i := range entries {
		populateEntryURLs(&entries[i])
	}

	distinctDays := make(map[string]bool)
	totalWords := 0
	for _, e := range entries {
		dStr := e.OccurredAt.Format("2006-01-02")
		distinctDays[dStr] = true
		totalWords += len([]rune(e.Content))
	}

	firstDateStr := ""
	lastDateStr := ""
	if len(entries) > 0 {
		firstDateStr = entries[0].OccurredAt.Format("2006-01-02")
		lastDateStr = entries[len(entries)-1].OccurredAt.Format("2006-01-02")
	}

	// 2. 第二幕：山川行迹 (Footprints 聚合)
	var footprints []models.Footprint
	db.DB.Where("visited_at >= ? AND visited_at < ?", startOfYear, endOfYear).
		Preload("Attachments").
		Order("visited_at ASC").
		Find(&footprints)

	for i := range footprints {
		populateFootprintURLs(&footprints[i])
	}

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
		Preload("Attachments").
		Order("fulfilled_at ASC").
		Find(&fulfilledWishes)

	for i := range fulfilledWishes {
		populateWishURLs(&fulfilledWishes[i])
	}

	var wantingWishCount int64
	db.DB.Model(&models.Wish{}).Where("status = ?", "wanting").Count(&wantingWishCount)

	var totalWishBudget, totalWishActual int64
	for _, w := range fulfilledWishes {
		totalWishBudget += w.Budget
		totalWishActual += w.ActualCost
	}

	// 4. 第四幕：浮光掠影 (四季高光与菲林相片)
	var springHighlight, summerHighlight, autumnHighlight, winterHighlight *models.LifeEntry
	var filmGallery []models.Attachment

	for i, e := range entries {
		m := e.OccurredAt.Month()
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

	// 截取最多 24 张精选照片作为长胶卷
	if len(filmGallery) > 24 {
		filmGallery = filmGallery[:24]
	}

	// 5. 第五幕：柴米油盐 (Transactions 聚合)
	var txs []models.Transaction
	db.DB.Where("occurred_at >= ? AND occurred_at < ?", startOfYear, endOfYear).Find(&txs)

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

	// 组装年鉴信封
	etag := fmt.Sprintf(`W/"almanac-%d-%d-%d"`, year, len(entries), len(txs))
	c.Header("ETag", etag)

	if match := c.GetHeader("If-None-Match"); match != "" && match == etag {
		c.Status(http.StatusNotModified)
		return
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
			"tx_count":         len(txs),
		},
		"atlas_journey": gin.H{
			"footprints":     footprints,
			"cities_visited": citiesVisited,
		},
		"wishes_milestones": gin.H{
			"fulfilled_wishes":   fulfilledWishes,
			"total_wish_budget":  totalWishBudget,
			"total_wish_actual":  totalWishActual,
			"wanting_wish_count": wantingWishCount,
		},
		"life_highlights": gin.H{
			"spring":       springHighlight,
			"summer":       summerHighlight,
			"autumn":       autumnHighlight,
			"winter":       winterHighlight,
			"film_gallery": filmGallery,
		},
		"financial_spectrum": gin.H{
			"total_income":       totalIncome,
			"total_expense":      totalExpense,
			"balance":            totalIncome - totalExpense,
			"wish_expenses":      wishExpenses,
			"wish_expense_ratio": wishRatio,
			"categories":         categories,
		},
	})
}
