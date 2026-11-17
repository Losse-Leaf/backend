package handlers

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
)

// DrawerCategoryItem 回忆抽屉消费分类统计项
type DrawerCategoryItem struct {
	Name   string `json:"name"`
	Amount int64  `json:"amount"`
}

// ProvinceVisitedCity 省级抽屉中记录的已访问城市简要统计
type ProvinceVisitedCity struct {
	CityCode   string `json:"city_code"`
	CityName   string `json:"city_name"`
	VisitCount int    `json:"visit_count"`
}

// memoryDrawerBundle 城市与省份回忆抽屉共用的查询聚合结果
type memoryDrawerBundle struct {
	footprints    []models.Footprint
	entries       []models.LifeEntry
	ledgerSummary gin.H
	firstVisitStr string
	latestVisit   string
}

// processDrawerFootprints 统一处理抽屉足迹相片签名、旅程花销统计并计算首访与最近到访日期
func processDrawerFootprints(footprints []models.Footprint, isAdmin bool) (string, string, []uint) {
	populateFootprintURLsBatch(footprints)
	populateFootprintExpenseStatsBatch(footprints, isAdmin)
	var firstVisit, latestVisit time.Time
	fpIDs := make([]uint, 0, len(footprints))
	for _, f := range footprints {
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
		"total_spent":       totalSpent,
		"transaction_count": expenseCount,
		"categories":        categories,
	}
}

// loadMemoryDrawerBundle 统一构建城市或省份回忆抽屉的三段聚合查询（足迹列表、关联散页、消费光谱）。
func loadMemoryDrawerBundle(
	cityClause string,
	cityArg string,
	isAdmin bool,
	shortKeywordFn func([]models.Footprint) string,
) memoryDrawerBundle {
	var footprints []models.Footprint
	fpQ := db.DB.Where(cityClause, cityArg)
	if !isAdmin {
		fpQ = fpQ.Where("is_private = ?", false)
	}
	fpQ.Order("visited_at DESC").Find(&footprints)
	if footprints == nil {
		footprints = []models.Footprint{}
	}

	shortKeyword := ""
	if shortKeywordFn != nil {
		shortKeyword = shortKeywordFn(footprints)
	}

	firstVisitStr, latestVisitStr, fpIDs := processDrawerFootprints(footprints, isAdmin)

	var entries []models.LifeEntry
	entryQuery := db.DB.Order("occurred_at DESC")
	if !isAdmin {
		entryQuery = entryQuery.Where("is_private = ?", false)
	}
	switch {
	case len(fpIDs) > 0 && shortKeyword != "":
		entryQuery = entryQuery.Where(cityClause+" OR footprint_id IN ? OR location LIKE ?", cityArg, fpIDs, "%"+shortKeyword+"%")
	case len(fpIDs) > 0:
		entryQuery = entryQuery.Where(cityClause+" OR footprint_id IN ?", cityArg, fpIDs)
	case shortKeyword != "":
		entryQuery = entryQuery.Where(cityClause+" OR location LIKE ?", cityArg, "%"+shortKeyword+"%")
	default:
		entryQuery = entryQuery.Where(cityClause, cityArg)
	}
	entryQuery.Limit(50).Find(&entries)
	if entries == nil {
		entries = []models.LifeEntry{}
	}

	populateEntryURLsBatch(entries)
	if !isAdmin {
		redactEmbeddedWishFinancesBatch(entries)
	}

	ledgerSummary := buildDrawerLedgerSummary(isAdmin, func() []models.Transaction {
		var txs []models.Transaction
		likePat := "%" + shortKeyword + "%"
		txQuery := db.DB.Where(cityClause, cityArg)
		switch {
		case len(fpIDs) > 0 && shortKeyword != "":
			txQuery = db.DB.Where(cityClause+" OR footprint_id IN ? OR note LIKE ? OR title LIKE ?", cityArg, fpIDs, likePat, likePat)
		case len(fpIDs) > 0:
			txQuery = db.DB.Where(cityClause+" OR footprint_id IN ?", cityArg, fpIDs)
		case shortKeyword != "":
			txQuery = db.DB.Where(cityClause+" OR note LIKE ? OR title LIKE ?", cityArg, likePat, likePat)
		}
		txQuery.Find(&txs)
		return txs
	})

	return memoryDrawerBundle{
		footprints:    footprints,
		entries:       entries,
		ledgerSummary: ledgerSummary,
		firstVisitStr: firstVisitStr,
		latestVisit:   latestVisitStr,
	}
}

// GetCityMemoryDrawer 城市回忆抽屉聚合接口 (RC-93 两级并行：履迹简志 + 菲林画廊 + 散页随笔 + 城市账单光谱)
func GetCityMemoryDrawer(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "footprints") {
		return
	}
	cityCode := strings.TrimSpace(c.Param("city_code"))
	if cityCode == "" {
		Error(c, http.StatusBadRequest, "城市代码必填")
		return
	}

	isAdmin := middleware.IsAdmin(c)
	var cityName string
	bundle := loadMemoryDrawerBundle("city_code = ?", cityCode, isAdmin, func(fps []models.Footprint) string {
		if len(fps) > 0 {
			cityName = fps[0].CityName
		}
		return strings.TrimSuffix(cityName, "市")
	})

	setAuthVaryHeader(c)
	Success(c, gin.H{
		"city_code": cityCode,
		"city_name": cityName,
		"visit_stats": gin.H{
			"total_visits":      len(bundle.footprints),
			"first_visited_at":  bundle.firstVisitStr,
			"latest_visited_at": bundle.latestVisit,
		},
		"footprints":     bundle.footprints,
		"entries":        bundle.entries,
		"ledger_summary": bundle.ledgerSummary,
	})
}

// GetProvinceMemoryDrawer 省份回忆抽屉聚合接口 (RC-91 / RC-93 两级并行：履迹简志 + 菲林画廊 + 散页随笔 + 省份账单光谱)
func GetProvinceMemoryDrawer(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "footprints") {
		return
	}
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

	bundle := loadMemoryDrawerBundle("city_code LIKE ?", provPrefix+"%", isAdmin, func([]models.Footprint) string {
		return provShort
	})

	cityMap := make(map[string]*ProvinceVisitedCity)
	cityOrder := make([]string, 0)
	for _, f := range bundle.footprints {
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

	setAuthVaryHeader(c)
	Success(c, gin.H{
		"province_adcode": provAdcode,
		"province_name":   provName,
		"visited_cities":  visitedCities,
		"visit_stats": gin.H{
			"city_count":        len(visitedCities),
			"total_visits":      len(bundle.footprints),
			"first_visited_at":  bundle.firstVisitStr,
			"latest_visited_at": bundle.latestVisit,
		},
		"footprints":     bundle.footprints,
		"entries":        bundle.entries,
		"ledger_summary": bundle.ledgerSummary,
	})
}
