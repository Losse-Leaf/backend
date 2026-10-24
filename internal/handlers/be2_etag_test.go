package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/logger"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// sqlCountingLogger 统计一次 handler 执行期间实际发出的 SQL 条数。
// 用它断言 304 路径只跑了那一条聚合 SQL，没有 COUNT + Find + 签名。
type sqlCountingLogger struct {
	logger.Interface
	count *atomic.Int64
}

func (l *sqlCountingLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	l.count.Add(1)
	l.Interface.Trace(ctx, begin, fc, err)
}

func installSQLCounter(t *testing.T) *atomic.Int64 {
	t.Helper()
	counter := &atomic.Int64{}
	old := db.DB.Logger
	db.DB.Logger = &sqlCountingLogger{Interface: old, count: counter}
	t.Cleanup(func() { db.DB.Logger = old })
	return counter
}

func seedEntryForCache(t *testing.T, title string, occurred time.Time) {
	t.Helper()
	if err := db.DB.Create(&models.LifeEntry{
		Type:       "life",
		Title:      title,
		Content:    title,
		OccurredAt: occurred,
	}).Error; err != nil {
		t.Fatalf("seed entry: %v", err)
	}
}

// BE-2 核心验收：五个列表的 304 都只允许 1 条 SQL，且响应体为空。
func TestBE2_List304UsesAtMostOneSQL(t *testing.T) {
	now := time.Now().In(time.Local)

	cases := []struct {
		name    string
		path    string
		handler gin.HandlerFunc
		seed    func(t *testing.T)
	}{
		{
			name:    "entries",
			path:    "/api/v1/entries?page=1&page_size=1",
			handler: GetLifeEntriesV1,
			seed: func(t *testing.T) {
				seedEntryForCache(t, "a", now.Add(-2*time.Hour))
				seedEntryForCache(t, "b", now.Add(-time.Hour))
			},
		},
		{
			name:    "transactions",
			path:    "/api/v1/transactions?year=2026",
			handler: GetTransactions,
			seed: func(t *testing.T) {
				if err := db.DB.Create(&models.Transaction{
					Amount: 100, Type: "expense", Category: "餐饮", OccurredAt: now,
				}).Error; err != nil {
					t.Fatalf("seed transaction: %v", err)
				}
			},
		},
		{
			name:    "projects",
			path:    "/api/v1/projects",
			handler: GetProjectsV1,
			seed: func(t *testing.T) {
				if err := db.DB.Create(&models.Project{Title: "项目"}).Error; err != nil {
					t.Fatalf("seed project: %v", err)
				}
			},
		},
		{
			name:    "wishes",
			path:    "/api/v1/wishes",
			handler: GetWishes,
			seed: func(t *testing.T) {
				if err := db.DB.Create(&models.Wish{Title: "心愿", Kind: "item"}).Error; err != nil {
					t.Fatalf("seed wish: %v", err)
				}
			},
		},
		{
			name:    "footprints",
			path:    "/api/v1/footprints",
			handler: GetFootprints,
			seed: func(t *testing.T) {
				if err := db.DB.Create(&models.Footprint{
					CityCode: "330100", CityName: "杭州", VisitedAt: now,
				}).Error; err != nil {
					t.Fatalf("seed footprint: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			setupTestFullDB(t)
			tc.seed(t)

			r := gin.New()
			routePath := tc.path
			if idx := strings.IndexByte(routePath, '?'); idx >= 0 {
				routePath = routePath[:idx]
			}
			r.GET(routePath, tc.handler)

			first := httptest.NewRecorder()
			r.ServeHTTP(first, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if first.Code != http.StatusOK {
				t.Fatalf("first request expected 200, got %d: %s", first.Code, first.Body.String())
			}
			etag := first.Header().Get("ETag")
			if etag == "" {
				t.Fatalf("first request missing ETag")
			}

			counter := installSQLCounter(t)
			second := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("If-None-Match", etag)
			r.ServeHTTP(second, req)

			if second.Code != http.StatusNotModified {
				t.Fatalf("expected 304, got %d: %s", second.Code, second.Body.String())
			}
			if second.Body.Len() != 0 {
				t.Fatalf("304 must not write a body, got %d bytes", second.Body.Len())
			}
			if n := counter.Load(); n > 1 {
				t.Fatalf("304 should use at most 1 SQL (aggregate only), used %d", n)
			}
		})
	}
}

// ETag 必须包含查询串哈希：total 与时间戳相同、仅 page 不同的两个请求不能共用 ETag。
func TestBE2_ETagIncludesQueryString(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	now := time.Now().In(time.Local)
	seedEntryForCache(t, "a", now)
	seedEntryForCache(t, "b", now.Add(-time.Hour))

	r := gin.New()
	r.GET("/api/v1/entries", GetLifeEntriesV1)

	getETag := func(path string) string {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s => %d", path, w.Code)
		}
		return w.Header().Get("ETag")
	}

	p1 := getETag("/api/v1/entries?page=1&page_size=1")
	p2 := getETag("/api/v1/entries?page=2&page_size=1")
	if p1 == "" || p2 == "" {
		t.Fatalf("missing ETag: p1=%q p2=%q", p1, p2)
	}
	if p1 == p2 {
		t.Fatalf("ETag must differ by page/query string, both were %s", p1)
	}
}

// If-Modified-Since 仍保留在「非按角色脱敏」的列表上（这里用记账），
// 但排在 ETag 之后，且命中时同样不 Find。
func TestBE2_IfModifiedSinceStillWorks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	if err := db.DB.Create(&models.Transaction{
		Amount: 100, Type: "expense", Category: "餐饮", OccurredAt: time.Now().Add(-time.Hour),
	}).Error; err != nil {
		t.Fatalf("seed transaction: %v", err)
	}

	r := gin.New()
	r.GET("/api/v1/transactions", GetTransactions)

	counter := installSQLCounter(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/transactions", nil)
	req.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 via If-Modified-Since, got %d", w.Code)
	}
	if n := counter.Load(); n > 1 {
		t.Fatalf("If-Modified-Since 304 should use at most 1 SQL, used %d", n)
	}
}

// 304 时不得触发任何附件签名/查询：响应体为空 + 只有 1 条聚合 SQL 已足够证明。
// 附带验证 200 响应字段未变（total/page/page_size/entries）。
func TestBE2_200ResponseShapeUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	seedEntryForCache(t, "shape", time.Now())

	r := gin.New()
	r.GET("/api/v1/entries", GetLifeEntriesV1)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/entries", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing data object")
	}
	for _, key := range []string{"total", "page", "page_size", "entries"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("200 response missing field %q", key)
		}
	}
	if int(data["total"].(float64)) != 1 {
		t.Fatalf("expected total=1 from reused aggregate, got %v", data["total"])
	}
}
