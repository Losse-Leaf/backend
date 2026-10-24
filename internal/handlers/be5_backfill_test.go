package handlers

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// BE-5 验收：启动回填可重复执行，且不会重复创建足迹。
func TestBE5_EntryFootprintBackfillIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	visited := time.Date(2026, 5, 1, 10, 0, 0, 0, time.Local)
	e := models.LifeEntry{
		Type:       "travel",
		Title:      "杭州散步",
		OccurredAt: visited,
		CityCode:   "330100",
		CityName:   "杭州",
	}
	if err := db.DB.Create(&e).Error; err != nil {
		t.Fatalf("create entry: %v", err)
	}

	EnsureEntryFootprintsSynced()
	EnsureEntryFootprintsSynced()

	var fpCount int64
	if err := db.DB.Model(&models.Footprint{}).Where("city_code = ?", "330100").Count(&fpCount).Error; err != nil {
		t.Fatalf("count footprints: %v", err)
	}
	if fpCount != 1 {
		t.Fatalf("回填应恰好创建 1 条足迹，实际 %d（必须幂等）", fpCount)
	}

	var reloaded models.LifeEntry
	if err := db.DB.First(&reloaded, e.ID).Error; err != nil {
		t.Fatalf("reload entry: %v", err)
	}
	if reloaded.FootprintID == nil || *reloaded.FootprintID == 0 {
		t.Fatalf("回填后散页应绑定足迹，实际 footprint_id=%v", reloaded.FootprintID)
	}

	// 再跑一轮：不新增足迹，也不改变绑定。
	EnsureEntryFootprintsSynced()
	var fpCount2 int64
	db.DB.Model(&models.Footprint{}).Where("city_code = ?", "330100").Count(&fpCount2)
	if fpCount2 != 1 {
		t.Fatalf("重复回填不得新增足迹，实际 %d", fpCount2)
	}
}

// 账目回填已有测试 TestEnsureFootprintTransactionsLinkedIsIdempotent；
// 这里补一条「分批路径（batchSize=2）也不重复挂载」的回归。
func TestBE5_TransactionBackfillBatchedIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	visited := time.Date(2026, 6, 1, 10, 0, 0, 0, time.Local)
	fp := models.Footprint{CityCode: "310100", CityName: "上海", VisitedAt: visited}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatalf("create footprint: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := db.DB.Create(&models.Transaction{
			Amount: 1000, Type: "expense", Category: "餐饮",
			OccurredAt: visited, CityCode: "310100",
		}).Error; err != nil {
			t.Fatalf("create transaction: %v", err)
		}
	}

	if linked, processed, err := linkUnlinkedTransactionsBatched("", 2); err != nil || linked != 5 || processed != 5 {
		t.Fatalf("first batch backfill: linked=%d processed=%d err=%v", linked, processed, err)
	}
	if linked, processed, err := linkUnlinkedTransactionsBatched("", 2); err != nil || linked != 0 || processed != 0 {
		t.Fatalf("second batch backfill must be idempotent: linked=%d processed=%d err=%v", linked, processed, err)
	}

	var linkedCount int64
	db.DB.Model(&models.Transaction{}).Where("footprint_id = ?", fp.ID).Count(&linkedCount)
	if linkedCount != 5 {
		t.Fatalf("expected 5 linked transactions, got %d", linkedCount)
	}
}
