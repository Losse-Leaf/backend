package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
)

// TestCompanionEntries 校验「所有二级对象都要在时间流里留下一条一级记录」这一契约。
//
// 后端历史行为不一致：心愿 / 造物创建时会建伴生记录但编辑不更新、删除留孤儿；
// 器物则从头到尾都没有伴生记录。这里逐条钉住期望行为，防止回退。
func TestCompanionEntries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	testMasterKey := "lldt_companion_device_token_1"
	seedTestAdminDevice(testMasterKey)

	// 器物接口要求主理人档案已初始化
	if err := db.DB.Create(&models.CuratorProfile{Name: "测试主理人"}).Error; err != nil {
		t.Fatalf("seed curator profile: %v", err)
	}

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		v1.POST("/wishes", CreateWish)
		v1.PUT("/wishes/:id", UpdateWish)
		v1.DELETE("/wishes/:id", DeleteWish)
	}
	write := v1.Group("")
	write.Use(middleware.AdminAuthRequired())
	{
		write.POST("/assets/gear", AddGearAsset)
		write.DELETE("/assets/gear/:id", DeleteGearAsset)
	}

	// countEntriesByWish 统计某心愿关联的伴生一级记录
	countEntriesByWish := func(wishID uint) int64 {
		var n int64
		db.DB.Model(&models.LifeEntry{}).Where("wish_id = ?", wishID).Count(&n)
		return n
	}
	entryTitleOf := func(wishID uint) string {
		var e models.LifeEntry
		if err := db.DB.Where("wish_id = ?", wishID).Order("id asc").First(&e).Error; err != nil {
			return ""
		}
		return e.Title
	}

	// ── 1. 心愿：创建 / 编辑 / 删除的伴生记录语义 ──────────────────────────
	t.Run("Wish companion entry lifecycle", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"title":  "补档的旧心愿",
			"kind":   "item",
			"budget": 10000,
			// 补档：许下时刻是三个月前
			"planted_at": time.Now().AddDate(0, -3, 0).Format("2006-01-02T15:04:05+08:00"),
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/wishes", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("create wish failed: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data models.Wish `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode wish: %v", err)
		}
		wishID := resp.Data.ID
		if n := countEntriesByWish(wishID); n != 1 {
			t.Fatalf("expected exactly 1 companion entry for a new wish, got %d", n)
		}
		if title := entryTitleOf(wishID); title != "许下心愿 · 补档的旧心愿" {
			t.Fatalf("unexpected companion entry title: %q", title)
		}

		// 补档：伴生记录的 occurred_at 必须落在许下的那一刻，而不是今天
		var planted models.LifeEntry
		db.DB.Where("wish_id = ?", wishID).First(&planted)
		if planted.OccurredAt.After(time.Now().Add(-48 * time.Hour)) {
			t.Fatalf("backfilled companion entry should be dated in the past, got %v", planted.OccurredAt)
		}

		// 编辑标题：伴生记录要同步，不能留着旧标题
		upBody, _ := json.Marshal(map[string]any{"title": "改过名字的心愿"})
		w2 := httptest.NewRecorder()
		req2, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/wishes/%d", wishID), bytes.NewBuffer(upBody))
		req2.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("update wish failed: %d %s", w2.Code, w2.Body.String())
		}
		if title := entryTitleOf(wishID); title != "许下心愿 · 改过名字的心愿" {
			t.Fatalf("companion entry title was not synced on edit, got %q", title)
		}
		if n := countEntriesByWish(wishID); n != 1 {
			t.Fatalf("editing a wish must not duplicate companion entries, got %d", n)
		}

		// 删除心愿：伴生记录要一并消失，不能留下孤儿
		w3 := httptest.NewRecorder()
		req3, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/wishes/%d", wishID), nil)
		r.ServeHTTP(w3, req3)
		if w3.Code != http.StatusOK {
			t.Fatalf("delete wish failed: %d %s", w3.Code, w3.Body.String())
		}
		if n := countEntriesByWish(wishID); n != 0 {
			t.Fatalf("deleting a wish must remove its companion entry, %d left", n)
		}
	})

	// ── 2. 器物：添置时必须建伴生记录，删除时必须清理 ──────────────────────
	t.Run("Gear companion entry lifecycle", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"name":          "测试用相机",
			"category":      "数码器材",
			"price_cents":   599900,
			"purchase_date": "2026-03-01",
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/assets/gear", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Device-Token", testMasterKey)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("add gear failed: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data models.GearItem `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode gear: %v", err)
		}
		gearID := resp.Data.ID
		if gearID == "" {
			t.Fatalf("gear id missing in response: %s", w.Body.String())
		}

		var n int64
		db.DB.Model(&models.LifeEntry{}).Where("gear_id = ?", gearID).Count(&n)
		if n != 1 {
			t.Fatalf("adding a gear must create exactly 1 companion entry, got %d", n)
		}

		var gearEntry models.LifeEntry
		db.DB.Where("gear_id = ?", gearID).First(&gearEntry)
		if gearEntry.Title != "添置器物 · 测试用相机" {
			t.Fatalf("unexpected gear companion title: %q", gearEntry.Title)
		}
		// 伴生记录要落在购入日期上（补档）
		if gearEntry.OccurredAt.Year() != 2026 || gearEntry.OccurredAt.Month() != time.March {
			t.Fatalf("gear companion entry should be dated at the purchase date, got %v", gearEntry.OccurredAt)
		}

		w2 := httptest.NewRecorder()
		req2, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/assets/gear/%s", gearID), nil)
		req2.Header.Set("X-Device-Token", testMasterKey)
		r.ServeHTTP(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("delete gear failed: %d %s", w2.Code, w2.Body.String())
		}
		db.DB.Model(&models.LifeEntry{}).Where("gear_id = ?", gearID).Count(&n)
		if n != 0 {
			t.Fatalf("deleting a gear must remove its companion entry, %d left", n)
		}
	})
}
