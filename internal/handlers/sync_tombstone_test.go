package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// P2-3 方案 1：purge 物理删除必须留下最小墓碑，且五类增量列表都能把墓碑
// 合成回对应模型的 JSON 行；全量列表与过期 since 不得注入墓碑。

func setupTombstoneRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	token := "lldt_sync_tombstone"
	seedTestAdminDevice(token)

	r := gin.New()
	r.GET("/api/v1/entries", GetLifeEntriesV1)
	r.GET("/api/v1/transactions", GetTransactions)
	r.GET("/api/v1/projects", GetProjectsV1)
	r.GET("/api/v1/wishes", GetWishes)
	r.GET("/api/v1/footprints", GetFootprints)

	r.DELETE("/api/v1/entries/:id", DeleteLifeEntry)
	r.DELETE("/api/v1/transactions/:id", DeleteTransaction)
	r.DELETE("/api/v1/projects/:id", DeleteProject)
	r.DELETE("/api/v1/wishes/:id", DeleteWish)
	r.DELETE("/api/v1/footprints/:id", DeleteFootprint)
	return r, token
}

func tombstoneRows(t *testing.T, w *httptest.ResponseRecorder, key string) []map[string]any {
	t.Helper()
	body := w.Body.Bytes()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v, body=%s", err, string(body))
	}
	if key == "" {
		var rows []map[string]any
		if err := json.Unmarshal(env.Data, &rows); err != nil {
			t.Fatalf("unmarshal array data: %v, body=%s", err, string(body))
		}
		return rows
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(env.Data, &obj); err != nil {
		t.Fatalf("unmarshal object data: %v, body=%s", err, string(body))
	}
	var rows []map[string]any
	if err := json.Unmarshal(obj[key], &rows); err != nil {
		t.Fatalf("unmarshal %s: %v, body=%s", key, err, string(body))
	}
	return rows
}

func findTombstoneRow(rows []map[string]any, id uint) map[string]any {
	for _, row := range rows {
		if raw, ok := row["id"].(float64); ok && uint(raw) == id {
			return row
		}
	}
	return nil
}

func sinceParam(t time.Time) string {
	return url.QueryEscape(t.UTC().Format(time.RFC3339))
}

func TestPurgeWritesSyncTombstones(t *testing.T) {
	type testCase struct {
		name       string
		entityType string
		listPath   string
		listKey    string
		seed       func() uint
		purgePath  func(uint) string
	}

	cases := []testCase{
		{
			name:       "entry",
			entityType: TombstoneEntityEntry,
			listPath:   "/api/v1/entries",
			listKey:    "entries",
			seed: func() uint {
				item := models.LifeEntry{Type: "life", Title: "待物理删除", OccurredAt: time.Now()}
				if err := db.DB.Create(&item).Error; err != nil {
					t.Fatalf("seed entry: %v", err)
				}
				return item.ID
			},
			purgePath: func(id uint) string { return "/api/v1/entries/" + u64(id) + "?purge=true" },
		},
		{
			name:       "transaction",
			entityType: TombstoneEntityTransaction,
			listPath:   "/api/v1/transactions",
			seed: func() uint {
				item := models.Transaction{Amount: 100, Type: "expense", Category: "餐饮", OccurredAt: time.Now()}
				if err := db.DB.Create(&item).Error; err != nil {
					t.Fatalf("seed transaction: %v", err)
				}
				return item.ID
			},
			purgePath: func(id uint) string { return "/api/v1/transactions/" + u64(id) + "?purge=true" },
		},
		{
			name:       "project",
			entityType: TombstoneEntityProject,
			listPath:   "/api/v1/projects",
			seed: func() uint {
				item := models.Project{Title: "待物理删除项目"}
				if err := db.DB.Create(&item).Error; err != nil {
					t.Fatalf("seed project: %v", err)
				}
				return item.ID
			},
			purgePath: func(id uint) string { return "/api/v1/projects/" + u64(id) + "?purge=true" },
		},
		{
			name:       "wish",
			entityType: TombstoneEntityWish,
			listPath:   "/api/v1/wishes",
			listKey:    "wishes",
			seed: func() uint {
				item := models.Wish{Title: "待物理删除心愿", Kind: "item", Status: "wanting"}
				if err := db.DB.Create(&item).Error; err != nil {
					t.Fatalf("seed wish: %v", err)
				}
				return item.ID
			},
			purgePath: func(id uint) string { return "/api/v1/wishes/" + u64(id) + "?purge=true" },
		},
		{
			name:       "footprint",
			entityType: TombstoneEntityFootprint,
			listPath:   "/api/v1/footprints",
			seed: func() uint {
				item := models.Footprint{CityCode: "330100", CityName: "杭州", VisitedAt: time.Now()}
				if err := db.DB.Create(&item).Error; err != nil {
					t.Fatalf("seed footprint: %v", err)
				}
				return item.ID
			},
			purgePath: func(id uint) string { return "/api/v1/footprints/" + u64(id) + "?purge=true" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, token := setupTombstoneRouter(t)
			id := tc.seed()
			oldSince := sinceParam(time.Now().Add(-2 * time.Hour))

			if w := doJSON(r, http.MethodDelete, tc.purgePath(id), token, nil); w.Code != http.StatusOK {
				t.Fatalf("purge failed: %d %s", w.Code, w.Body.String())
			}

			// 1) 增量列表必须带回合成墓碑
			w := doJSON(r, http.MethodGet, tc.listPath+"?since="+oldSince, token, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("incremental list failed: %d %s", w.Code, w.Body.String())
			}
			rows := tombstoneRows(t, w, tc.listKey)
			row := findTombstoneRow(rows, id)
			if row == nil {
				t.Fatalf("expected synthetic tombstone for id=%d, rows=%v", id, rows)
			}
			if row["deleted_at"] == nil {
				t.Fatalf("expected deleted_at on synthetic tombstone, row=%v", row)
			}
			cursorRaw := w.Header().Get(SyncCursorHeader)
			if cursorRaw == "" {
				t.Fatalf("expected %s header on incremental response", SyncCursorHeader)
			}
			cursor, err := time.Parse(time.RFC3339, cursorRaw)
			if err != nil {
				t.Fatalf("cursor %q is not RFC3339: %v", cursorRaw, err)
			}
			deletedRaw, _ := row["deleted_at"].(string)
			deletedAt, err := time.Parse(time.RFC3339Nano, deletedRaw)
			if err != nil {
				t.Fatalf("deleted_at %q is not RFC3339: %v", deletedRaw, err)
			}
			// 游标头是秒级 RFC3339，墓碑时间带亚秒；只要游标落在同一秒内，
			// 下一轮 `>=` 过滤仍会带回墓碑，不会漏删。
			if cursor.Before(deletedAt.Truncate(time.Second)) {
				t.Fatalf("cursor %v must cover tombstone second %v", cursor, deletedAt)
			}

			// 2) 全量列表不得注入墓碑（行已被物理删除）
			w = doJSON(r, http.MethodGet, tc.listPath, token, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("full list failed: %d %s", w.Code, w.Body.String())
			}
			if row := findTombstoneRow(tombstoneRows(t, w, tc.listKey), id); row != nil {
				t.Fatalf("full list must not contain tombstone, row=%v", row)
			}

			// 3) 过期 since（晚于墓碑）不得再注入
			futureSince := sinceParam(time.Now().Add(2 * time.Hour))
			w = doJSON(r, http.MethodGet, tc.listPath+"?since="+futureSince, token, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("future incremental list failed: %d %s", w.Code, w.Body.String())
			}
			if row := findTombstoneRow(tombstoneRows(t, w, tc.listKey), id); row != nil {
				t.Fatalf("future since must not contain tombstone, row=%v", row)
			}

			// 4) 墓碑表里确实有一条记录
			var count int64
			if err := db.DB.Model(&models.SyncTombstone{}).
				Where("entity_type = ? AND entity_id = ?", tc.entityType, id).
				Count(&count).Error; err != nil {
				t.Fatalf("count tombstones: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected 1 tombstone for %s/%d, got %d", tc.entityType, id, count)
			}
		})
	}
}

// 级联物理删除：项目 purge 连带散页、足迹 purge 连带首生散页、
// 散页 purge 连带首生足迹，都必须逐条留下墓碑。
func TestPurgeCascadeWritesSyncTombstones(t *testing.T) {
	r, token := setupTombstoneRouter(t)
	oldSince := sinceParam(time.Now().Add(-2 * time.Hour))

	project := models.Project{Title: "级联项目"}
	if err := db.DB.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	entryA := models.LifeEntry{Type: "project", Title: "项目散页A", ProjectID: &project.ID, OccurredAt: time.Now()}
	entryB := models.LifeEntry{Type: "project", Title: "项目散页B", ProjectID: &project.ID, OccurredAt: time.Now()}
	if err := db.DB.Create(&entryA).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&entryB).Error; err != nil {
		t.Fatal(err)
	}

	if w := doJSON(r, http.MethodDelete, "/api/v1/projects/"+u64(project.ID)+"?purge=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("purge project failed: %d %s", w.Code, w.Body.String())
	}

	w := doJSON(r, http.MethodGet, "/api/v1/entries?since="+oldSince, token, nil)
	rows := tombstoneRows(t, w, "entries")
	for _, id := range []uint{entryA.ID, entryB.ID} {
		row := findTombstoneRow(rows, id)
		if row == nil || row["deleted_at"] == nil {
			t.Fatalf("expected cascade entry tombstone for id=%d, rows=%v", id, rows)
		}
	}

	// 足迹 purge 连带首生散页
	entryC := models.LifeEntry{Type: "travel", Title: "足迹首生散页", OccurredAt: time.Now()}
	if err := db.DB.Create(&entryC).Error; err != nil {
		t.Fatal(err)
	}
	fp := models.Footprint{CityCode: "330100", CityName: "杭州", VisitedAt: time.Now(), LifeEntryID: &entryC.ID}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodDelete, "/api/v1/footprints/"+u64(fp.ID)+"?purge=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("purge footprint failed: %d %s", w.Code, w.Body.String())
	}

	w = doJSON(r, http.MethodGet, "/api/v1/entries?since="+oldSince, token, nil)
	if row := findTombstoneRow(tombstoneRows(t, w, "entries"), entryC.ID); row == nil {
		t.Fatalf("expected cascade entry tombstone for footprint first-born entry id=%d", entryC.ID)
	}

	// 散页 purge 连带首生足迹
	entryD := models.LifeEntry{Type: "travel", Title: "将连带删除足迹", OccurredAt: time.Now()}
	if err := db.DB.Create(&entryD).Error; err != nil {
		t.Fatal(err)
	}
	fpD := models.Footprint{CityCode: "310100", CityName: "上海", VisitedAt: time.Now(), LifeEntryID: &entryD.ID}
	if err := db.DB.Create(&fpD).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodDelete, "/api/v1/entries/"+u64(entryD.ID)+"?purge=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("purge entry failed: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(r, http.MethodGet, "/api/v1/footprints?since="+oldSince, token, nil)
	if row := findTombstoneRow(tombstoneRows(t, w, ""), fpD.ID); row == nil {
		t.Fatalf("expected cascade footprint tombstone for id=%d", fpD.ID)
	}
}

// 增量请求跳过 ETag 304：否则墓碑会被 304 永远挡住。
func TestIncrementalListSkipsETag304(t *testing.T) {
	r, token := setupTombstoneRouter(t)
	entry := models.LifeEntry{Type: "life", Title: "ETag 墓碑", OccurredAt: time.Now()}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodDelete, "/api/v1/entries/"+u64(entry.ID)+"?purge=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("purge failed: %d %s", w.Code, w.Body.String())
	}

	// 全量请求仍走 ETag 协商，应带回 ETag。
	full := doJSON(r, http.MethodGet, "/api/v1/entries", token, nil)
	if full.Code != http.StatusOK {
		t.Fatalf("full list failed: %d %s", full.Code, full.Body.String())
	}
	etag := full.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("expected ETag header on full list")
	}

	oldSince := sinceParam(time.Now().Add(-2 * time.Hour))
	path := "/api/v1/entries?since=" + oldSince

	// 增量请求完全跳过协商：即使带上全量 ETag 也必须是 200，且响应不写 ETag。
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Device-Token", token)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	r.ServeHTTP(second, req)
	if second.Code == http.StatusNotModified {
		t.Fatalf("incremental request must not be short-circuited by ETag 304")
	}
	if second.Code != http.StatusOK {
		t.Fatalf("incremental list failed: %d %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("ETag"); got != "" {
		t.Fatalf("incremental response should skip ETag negotiation, got ETag %q", got)
	}
	if row := findTombstoneRow(tombstoneRows(t, second, "entries"), entry.ID); row == nil {
		t.Fatalf("tombstone must be present even when If-None-Match matches")
	}
}
