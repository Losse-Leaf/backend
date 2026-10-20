package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// MOB-P1-35：三端统一的「省略即默认」契约
//
// Android 端把 kotlinx.serialization 切到 encodeDefaults = false 后，
// 等于默认值的字段不会再出现在请求体里（type = "life" 会被省略）。
// 服务端必须把「省略」与「显式给出默认值」当作同一件事，
// 否则省略 type 的合法请求会被存成空类型 —— 而 type 是**筛选维度**，一旦为空记录就"消失"了。
//
// 这组用例守住这条契约：macOS(Swift Codable) 与 Web(JSON.stringify) 本来就在省略空值，
// 服务端的行为对三端完全一致。

func postJSON(t *testing.T, r *gin.Engine, path string, payload map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, path, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestOmittedFieldsFallBackToDocumentedDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)

	r := gin.New()
	r.POST("/api/v1/entries", CreateLifeEntry)
	r.POST("/api/v1/transactions", CreateTransaction)
	r.POST("/api/v1/wishes", CreateWish)

	t.Run("条目省略 type 时默认 life", func(t *testing.T) {
		w := postJSON(t, r, "/api/v1/entries", map[string]interface{}{
			"title":       "省略类型的随笔",
			"content":     "内容",
			"occurred_at": "2026-10-10T08:00:00Z",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var entry models.LifeEntry
		if err := db.DB.Order("id DESC").First(&entry).Error; err != nil {
			t.Fatalf("load entry: %v", err)
		}
		if entry.Type != "life" {
			t.Fatalf("expected default type 'life', got %q", entry.Type)
		}
	})

	t.Run("记账省略 type 时默认 expense", func(t *testing.T) {
		w := postJSON(t, r, "/api/v1/transactions", map[string]interface{}{
			"amount":         100,
			"category":       "餐饮",
			"title":          "省略类型的记账",
			"occurred_at":    "2026-10-10T08:00:00Z",
			"payment_method": "",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var tx models.Transaction
		if err := db.DB.Order("id DESC").First(&tx).Error; err != nil {
			t.Fatalf("load transaction: %v", err)
		}
		if tx.Type != "expense" {
			t.Fatalf("expected default type 'expense', got %q", tx.Type)
		}
	})

	t.Run("心愿省略 kind 时默认 item", func(t *testing.T) {
		w := postJSON(t, r, "/api/v1/wishes", map[string]interface{}{
			"title": "省略类型的心愿",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var wish models.Wish
		if err := db.DB.Order("id DESC").First(&wish).Error; err != nil {
			t.Fatalf("load wish: %v", err)
		}
		if wish.Kind != "item" {
			t.Fatalf("expected default kind 'item', got %q", wish.Kind)
		}
	})
}
