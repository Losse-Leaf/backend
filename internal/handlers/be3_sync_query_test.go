package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

type explainRow struct {
	ID     int
	Parent int
	Detail string
}

func queryPlan(t *testing.T, query func(tx *gorm.DB) *gorm.DB) string {
	t.Helper()
	sql := db.DB.ToSQL(query)
	if strings.TrimSpace(sql) == "" {
		t.Fatalf("ToSQL returned empty SQL")
	}
	var rows []explainRow
	if err := db.DB.Raw("EXPLAIN QUERY PLAN " + sql).Scan(&rows).Error; err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN failed: %v\nSQL: %s", err, sql)
	}
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(row.Detail)
		b.WriteString("\n")
	}
	return b.String()
}

func seedManyEntries(t *testing.T, n int) {
	t.Helper()
	batch := make([]models.LifeEntry, 0, n)
	base := time.Now().Add(-48 * time.Hour)
	for i := 0; i < n; i++ {
		batch = append(batch, models.LifeEntry{
			Type:       "life",
			Title:      "seed",
			OccurredAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	if err := db.DB.CreateInBatches(batch, 200).Error; err != nil {
		t.Fatalf("seed entries: %v", err)
	}
}

// BE-3 验收：带 since 的 entries/transactions 查询计划必须走 updated_at 索引
// （或 MULTI-INDEX OR），不能退化成沿 occurred_at 的全表扫描。
func TestBE3_SyncSinceQueryPlanUsesUpdatedAtIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	seedManyEntries(t, 1000)
	for i := 0; i < 200; i++ {
		if err := db.DB.Create(&models.Transaction{
			Amount: 100, Type: "expense", Category: "餐饮",
			OccurredAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}).Error; err != nil {
			t.Fatalf("seed transaction: %v", err)
		}
	}
	since := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)

	t.Run("entries", func(t *testing.T) {
		plan := queryPlan(t, func(tx *gorm.DB) *gorm.DB {
			var entries []models.LifeEntry
			q := applySyncSince(tx.Model(&models.LifeEntry{}).Order("occurred_at DESC, id DESC"), &models.LifeEntry{}, since)
			return q.Limit(50).Find(&entries)
		})
		if !strings.Contains(plan, "idx_life_entries_updated_at") && !strings.Contains(plan, "MULTI-INDEX OR") {
			t.Fatalf("entries plan should use updated_at index / MULTI-INDEX OR, got:\n%s", plan)
		}
		if strings.Contains(plan, "SCAN life_entries\n") {
			t.Fatalf("entries plan must not full-scan life_entries, got:\n%s", plan)
		}
	})

	t.Run("transactions", func(t *testing.T) {
		plan := queryPlan(t, func(tx *gorm.DB) *gorm.DB {
			var txs []models.Transaction
			q := applySyncSince(tx.Model(&models.Transaction{}).Order("occurred_at DESC, id DESC"), &models.Transaction{}, since)
			return q.Limit(50).Find(&txs)
		})
		if !strings.Contains(plan, "idx_transactions_updated_at") && !strings.Contains(plan, "MULTI-INDEX OR") {
			t.Fatalf("transactions plan should use updated_at index / MULTI-INDEX OR, got:\n%s", plan)
		}
		if strings.Contains(plan, "SCAN transactions\n") {
			t.Fatalf("transactions plan must not full-scan transactions, got:\n%s", plan)
		}
	})
}

// since 只返回变更行（含软删墓碑），且边界 >= 保留。
func TestBE3_SinceReturnsOnlyChangedRowsIncludingTombstones(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	old := models.LifeEntry{Type: "life", Title: "old", OccurredAt: base}
	changed := models.LifeEntry{Type: "life", Title: "changed", OccurredAt: base.Add(10 * time.Minute)}
	tombstone := models.LifeEntry{Type: "life", Title: "tombstone", OccurredAt: base.Add(20 * time.Minute)}
	for _, e := range []*models.LifeEntry{&old, &changed, &tombstone} {
		if err := db.DB.Create(e).Error; err != nil {
			t.Fatalf("seed entry: %v", err)
		}
	}

	changedAt := base.Add(time.Hour)
	deletedAt := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	db.DB.Model(&models.LifeEntry{}).Where("id = ?", old.ID).UpdateColumn("updated_at", base.Add(-time.Hour))
	db.DB.Model(&models.LifeEntry{}).Where("id = ?", changed.ID).UpdateColumn("updated_at", changedAt)
	db.DB.Unscoped().Model(&models.LifeEntry{}).Where("id = ?", tombstone.ID).UpdateColumn("deleted_at", deletedAt)

	adminKey := "lldt_sync_tombstone_key"
	seedTestAdminDevice(adminKey)

	r := gin.New()
	r.GET("/api/v1/entries", GetLifeEntriesV1)

	listIDs := func(since time.Time) []int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/entries?since="+url.QueryEscape(since.Format(time.RFC3339Nano)), nil)
		req.Header.Set("Authorization", "Bearer "+adminKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("since=%s request failed: %d %s", since, w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		data := resp["data"].(map[string]interface{})
		raw := data["entries"].([]interface{})
		ids := make([]int, 0, len(raw))
		for _, item := range raw {
			ids = append(ids, int(item.(map[string]interface{})["id"].(float64)))
		}
		return ids
	}

	ids := listIDs(base)
	if !containsID(ids, int(changed.ID)) {
		t.Fatalf("changed row must be returned, got %v", ids)
	}
	if !containsID(ids, int(tombstone.ID)) {
		t.Fatalf("soft-deleted tombstone must be returned, got %v", ids)
	}
	if containsID(ids, int(old.ID)) {
		t.Fatalf("unchanged old row must not be returned, got %v", ids)
	}

	// 边界：since 恰好等于 changed.updated_at 时仍必须带回（>=）。
	boundaryIDs := listIDs(changedAt)
	if !containsID(boundaryIDs, int(changed.ID)) {
		t.Fatalf("inclusive boundary failed: since==updated_at should return row %d, got %v", changed.ID, boundaryIDs)
	}
}
