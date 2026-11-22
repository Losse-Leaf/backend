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
			"query":   "",
			"entries": []models.LifeEntry{},
			"wishes":  []models.Wish{},
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
		entryQ := db.DB.Where("title LIKE ? OR content LIKE ? OR location LIKE ?", likePat, likePat, likePat)
		if !isAdmin {
			entryQ = entryQ.Where("is_private = ?", false)
		}
		entryQ.Order("occurred_at DESC, id DESC").Limit(limit).Find(&entries)
		populateEntryURLsBatch(entries)
		if !isAdmin {
			redactEmbeddedWishFinancesBatch(entries)
		}
	}

	wishes := []models.Wish{}
	if isAdmin || pub["wishes"] {
		wishQ := db.DB.Where("title LIKE ? OR note LIKE ? OR spec LIKE ?", likePat, likePat, likePat)
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

	setAuthVaryHeader(c)
	Success(c, gin.H{
		"query":   q,
		"entries": entries,
		"wishes":  wishes,
	})
}
