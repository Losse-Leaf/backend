package handlers

import (
	"strconv"
	"strings"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

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
	isAdmin := middleware.IsAdmin(c)
	pub := GetPublicModulesMap()

	entries := []models.LifeEntry{}
	if isAdmin || pub["entries"] {
		entryQ := db.DB.Where("title LIKE ? OR content LIKE ? OR location LIKE ? OR city_name LIKE ?", likePat, likePat, likePat, likePat)
		if !isAdmin {
			entryQ = entryQ.Where("is_private = ?", false)
		}
		entryQ.Order("occurred_at DESC, id DESC").Limit(limit).Find(&entries)
		populateEntryURLsBatch(entries)
		if !isAdmin {
			redactEmbeddedWishFinancesBatch(entries)
		}
	}

	footprints := []models.Footprint{}
	if isAdmin || pub["footprints"] {
		fpQ := db.DB.Where("city_name LIKE ? OR title LIKE ? OR note LIKE ?", likePat, likePat, likePat)
		if !isAdmin {
			fpQ = fpQ.Where("is_private = ?", false)
		}
		fpQ.Order("visited_at DESC, id DESC").Limit(limit).Find(&footprints)
		populateFootprintURLsBatch(footprints)
	}

	wishes := []models.Wish{}
	if isAdmin || pub["wishes"] {
		wishQ := db.DB.Where("title LIKE ? OR note LIKE ? OR spec LIKE ? OR city_name LIKE ?", likePat, likePat, likePat, likePat)
		if !isAdmin {
			wishQ = wishQ.Where("is_private = ?", false)
		}
		wishQ.Order("created_at DESC, id DESC").Limit(limit).Find(&wishes)
		populateWishURLsBatch(wishes)
		if !isAdmin {
			for i := range wishes {
				redactEmbeddedWishFinances(&wishes[i])
			}
		}
	}

	projects := []models.Project{}
	if isAdmin || pub["projects"] {
		projQ := db.DB.Where("title LIKE ? OR subtitle LIKE ? OR description LIKE ? OR story LIKE ? OR category LIKE ?", likePat, likePat, likePat, likePat, likePat)
		if !isAdmin {
			projQ = projQ.Where("is_private = ?", false)
		}
		projQ.Order("`order` ASC, id DESC").Limit(limit).Find(&projects)
	}

	setAuthVaryHeader(c)
	Success(c, gin.H{
		"query":      q,
		"entries":    entries,
		"footprints": footprints,
		"wishes":     wishes,
		"projects":   projects,
	})
}
