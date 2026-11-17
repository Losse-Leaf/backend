package handlers

import (
	"sync"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

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
	setAuthVaryHeader(c)
	isAdmin := middleware.IsAdmin(c)
	now := time.Now()
	nowBeijing := now.In(config.BeijingLocation)

	if !isAdmin {
		pub := GetPublicModulesMap()
		var totalEntries int64
		if pub["entries"] {
			db.DB.Model(&models.LifeEntry{}).Where("is_private = ?", false).Count(&totalEntries)
		}
		var totalPhotos int64
		if pub["entries"] {
			db.DB.Model(&models.Attachment{}).
				Joins("JOIN life_entries ON life_entries.id = attachments.entry_id").
				Where("attachments.mime_type LIKE ? AND life_entries.deleted_at IS NULL AND life_entries.is_private = ?", "image/%", false).
				Count(&totalPhotos)
		}
		var totalProjects int64
		if pub["projects"] {
			db.DB.Model(&models.Project{}).Where("is_private = ?", false).Count(&totalProjects)
		}
		Success(c, gin.H{
			"total_entries":  totalEntries,
			"total_photos":   totalPhotos,
			"total_projects": totalProjects,
			"month_expense":  0,
			"current_year":   nowBeijing.Year(),
			"current_month":  int(nowBeijing.Month()),
		})
		return
	}

	statsCacheMutex.RLock()
	if cachedStats != nil && now.Sub(cachedStatsTime) < 30*time.Second {
		snap := *cachedStats
		statsCacheMutex.RUnlock()
		Success(c, gin.H{
			"total_entries":  snap.TotalEntries,
			"total_photos":   snap.TotalPhotos,
			"total_projects": snap.TotalProjects,
			"month_expense":  snap.MonthExpense,
			"current_year":   snap.CurrentYear,
			"current_month":  snap.CurrentMonth,
		})
		return
	}
	statsCacheMutex.RUnlock()

	var totalEntries int64
	db.DB.Model(&models.LifeEntry{}).Count(&totalEntries)

	var totalPhotos int64
	db.DB.Model(&models.Attachment{}).Where("mime_type LIKE ?", "image/%").Count(&totalPhotos)

	var totalProjects int64
	db.DB.Model(&models.Project{}).Count(&totalProjects)

	start := time.Date(nowBeijing.Year(), nowBeijing.Month(), 1, 0, 0, 0, 0, config.BeijingLocation)
	end := start.AddDate(0, 1, 0)

	var res struct {
		Total int64
	}
	db.DB.Model(&models.Transaction{}).
		Select("COALESCE(SUM(CASE WHEN source = 'installment_repay' THEN COALESCE(fee_amount, 0) ELSE amount END), 0) as total").
		Where("type = 'expense' AND occurred_at >= ? AND occurred_at < ?", start, end).
		Scan(&res)

	snap := statsSnapshot{
		TotalEntries:  totalEntries,
		TotalPhotos:   totalPhotos,
		TotalProjects: totalProjects,
		MonthExpense:  res.Total,
		CurrentYear:   nowBeijing.Year(),
		CurrentMonth:  int(nowBeijing.Month()),
	}

	statsCacheMutex.Lock()
	cachedStats = &snap
	cachedStatsTime = now
	statsCacheMutex.Unlock()

	Success(c, gin.H{
		"total_entries":  snap.TotalEntries,
		"total_photos":   snap.TotalPhotos,
		"total_projects": snap.TotalProjects,
		"month_expense":  snap.MonthExpense,
		"current_year":   snap.CurrentYear,
		"current_month":  snap.CurrentMonth,
	})
}
