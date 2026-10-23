package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// sqlCountLogger 只统计实际执行的 SQL 条数（不打印日志）
type sqlCountLogger struct {
	count int
}

func (l *sqlCountLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }
func (l *sqlCountLogger) Info(context.Context, string, ...interface{})     {}
func (l *sqlCountLogger) Warn(context.Context, string, ...interface{})     {}
func (l *sqlCountLogger) Error(context.Context, string, ...interface{})    {}
func (l *sqlCountLogger) Trace(context.Context, time.Time, func() (string, int64), error) {
	l.count++
}

// seedFootprintsWithExpenses 构造 3 条足迹与多笔交易：
//
//	旅程 A（最新）：2 笔支出 + 1 笔收入（收入不得计入）
//	旅程 B（居中）：1 笔支出
//	旅程 C（最旧）：没有交易
//	另有一笔未挂载支出（不得计入任何旅程）
func seedFootprintsWithExpenses(t *testing.T) []models.Footprint {
	t.Helper()

	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	footprints := []models.Footprint{
		{CityCode: "110100", CityName: "北京市", Title: "旅程 A", VisitedAt: base},
		{CityCode: "310100", CityName: "上海市", Title: "旅程 B", VisitedAt: base.AddDate(0, 0, 2)},
		{CityCode: "330100", CityName: "杭州市", Title: "旅程 C", VisitedAt: base.AddDate(0, 0, 1)},
	}
	for i := range footprints {
		if err := db.DB.Create(&footprints[i]).Error; err != nil {
			t.Fatalf("create footprint: %v", err)
		}
	}

	mkTx := func(amount int64, typ, title string, footprintID *uint) {
		t.Helper()
		tx := models.Transaction{
			Amount:      amount,
			Type:        typ,
			Category:    "餐饮",
			Title:       title,
			OccurredAt:  base,
			FootprintID: footprintID,
		}
		if err := db.DB.Create(&tx).Error; err != nil {
			t.Fatalf("create transaction: %v", err)
		}
	}

	fpA, fpB := footprints[0].ID, footprints[1].ID
	mkTx(1000, "expense", "A-午餐", &fpA)
	mkTx(2500, "expense", "A-晚餐", &fpA)
	mkTx(9999, "income", "A-回血", &fpA)
	mkTx(4200, "expense", "B-门票", &fpB)
	mkTx(777, "expense", "未挂载支出", nil)

	return footprints
}

// 一条聚合查询即可回填全部旅程的花销，且统计口径正确。
func TestPopulateFootprintExpenseStatsBatchSingleQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	seedFootprintsWithExpenses(t)

	var loaded []models.Footprint
	if err := db.DB.Order("id ASC").Find(&loaded).Error; err != nil {
		t.Fatalf("load footprints: %v", err)
	}

	counter := &sqlCountLogger{}
	db.DB = db.DB.Session(&gorm.Session{Logger: counter})

	populateFootprintExpenseStatsBatch(loaded, true)

	if counter.count != 1 {
		t.Fatalf("3 条足迹的花销统计应只执行 1 条 SQL（GROUP BY），实际 %d 条", counter.count)
	}

	stats := make(map[uint]models.Footprint, len(loaded))
	for _, fp := range loaded {
		stats[fp.ID] = fp
	}
	// 旅程 A：1000 + 2500 = 3500，2 笔；收入 9999 不计入
	if got := stats[loaded[0].ID]; got.TotalSpent != 3500 || got.TransactionCount != 2 {
		t.Errorf("旅程 A 统计错误：期望 total=3500 count=2，实际 total=%d count=%d", got.TotalSpent, got.TransactionCount)
	}
	// 旅程 B：4200，1 笔
	if got := stats[loaded[1].ID]; got.TotalSpent != 4200 || got.TransactionCount != 1 {
		t.Errorf("旅程 B 统计错误：期望 total=4200 count=1，实际 total=%d count=%d", got.TotalSpent, got.TransactionCount)
	}
	// 旅程 C：没有交易
	if got := stats[loaded[2].ID]; got.TotalSpent != 0 || got.TransactionCount != 0 {
		t.Errorf("旅程 C 统计错误：期望全 0，实际 total=%d count=%d", got.TotalSpent, got.TransactionCount)
	}
}

// 管理员走列表接口时应批量回填统计数据；非管理员不得看到花销。
func TestGetFootprintsExpenseStatsPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	footprints := seedFootprintsWithExpenses(t)

	r := gin.New()
	r.GET("/admin/footprints", func(c *gin.Context) {
		c.Set("auth_type", "device")
		GetFootprints(c)
	})
	r.GET("/public/footprints", GetFootprints)

	// 管理员：3 条足迹都带统计
	req := httptest.NewRequest(http.MethodGet, "/admin/footprints", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var adminResp struct {
		Data []models.Footprint `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &adminResp); err != nil {
		t.Fatalf("decode admin response: %v", err)
	}
	if len(adminResp.Data) != 3 {
		t.Fatalf("管理员应看到 3 条足迹，实际 %d", len(adminResp.Data))
	}
	stats := make(map[uint]models.Footprint)
	for _, fp := range adminResp.Data {
		stats[fp.ID] = fp
	}
	if got := stats[footprints[0].ID]; got.TotalSpent != 3500 || got.TransactionCount != 2 {
		t.Errorf("管理员列表中的旅程 A 统计错误：total=%d count=%d", got.TotalSpent, got.TransactionCount)
	}
	if got := stats[footprints[1].ID]; got.TotalSpent != 4200 || got.TransactionCount != 1 {
		t.Errorf("管理员列表中的旅程 B 统计错误：total=%d count=%d", got.TotalSpent, got.TransactionCount)
	}

	// 非管理员：统计字段必须保持零值
	req = httptest.NewRequest(http.MethodGet, "/public/footprints", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var publicResp struct {
		Data []models.Footprint `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &publicResp); err != nil {
		t.Fatalf("decode public response: %v", err)
	}
	for _, fp := range publicResp.Data {
		if fp.TotalSpent != 0 || fp.TransactionCount != 0 {
			t.Errorf("非管理员不得返回花销统计：footprint=%d total=%d count=%d", fp.ID, fp.TotalSpent, fp.TransactionCount)
		}
	}
}

// limit 仅在显式传入时生效：默认仍返回原数组；limit>0 时返回
// {footprints, has_more}，顺序仍为 visited_at DESC, id DESC。
func TestGetFootprintsLimitAndHasMore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	footprints := seedFootprintsWithExpenses(t)

	r := gin.New()
	r.GET("/api/v1/footprints", GetFootprints)

	// limit=2：返回最新的两条，has_more=true
	req := httptest.NewRequest(http.MethodGet, "/api/v1/footprints?limit=2", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var paged struct {
		Data struct {
			Footprints []models.Footprint `json:"footprints"`
			HasMore    bool               `json:"has_more"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &paged); err != nil {
		t.Fatalf("decode paged response: %v", err)
	}
	if len(paged.Data.Footprints) != 2 {
		t.Fatalf("limit=2 应返回 2 条，实际 %d", len(paged.Data.Footprints))
	}
	if !paged.Data.HasMore {
		t.Error("还有第 3 条时 has_more 必须为 true")
	}
	wantOrder := []uint{footprints[1].ID, footprints[2].ID} // 上海(10-03) → 杭州(10-02)
	for i, want := range wantOrder {
		if paged.Data.Footprints[i].ID != want {
			t.Errorf("分页顺序错误：第 %d 条期望 %d，实际 %d", i, want, paged.Data.Footprints[i].ID)
		}
	}

	// limit=3：刚好取完，has_more=false
	req = httptest.NewRequest(http.MethodGet, "/api/v1/footprints?limit=3", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &paged); err != nil {
		t.Fatalf("decode paged response: %v", err)
	}
	if len(paged.Data.Footprints) != 3 || paged.Data.HasMore {
		t.Errorf("limit=3 应返回 3 条且 has_more=false，实际 len=%d has_more=%v",
			len(paged.Data.Footprints), paged.Data.HasMore)
	}

	// 不传 limit / limit=0：保持原数组契约，返回全部
	for _, path := range []string{"/api/v1/footprints", "/api/v1/footprints?limit=0"} {
		req = httptest.NewRequest(http.MethodGet, path, nil)
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var full struct {
			Data []models.Footprint `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
			t.Fatalf("%s: 默认响应应仍是数组: %v", path, err)
		}
		if len(full.Data) != 3 {
			t.Errorf("%s 应返回全部 3 条，实际 %d", path, len(full.Data))
		}
		if len(full.Data) > 0 && full.Data[0].ID != footprints[1].ID {
			t.Errorf("%s 顺序错误：首条期望 %d，实际 %d", path, footprints[1].ID, full.Data[0].ID)
		}
	}
}
