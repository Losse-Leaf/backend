package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

func boolPtrV(v bool) *bool { return &v }

// 「显式才破坏」——三条数据安全契约的回归测试。
//
// 背景：这些字段的语义从「值」改成了「意图」，但**老客户端不会改**：
//   - mac v1.8.1 的 `isInstallment` 是非可选 Bool → 必定发 `false`
//   - mac 的 `attachmentIds` 是非可选 [Int] → 必定发 `[]`
//   - mac 的心愿编辑弹窗**根本没有附件 UI**
//
// 若把「false / 空数组」直接当成「取消分期 / 全部解绑」，
// 老用户**只是改个字**就会静默毁掉自己的分期负债与照片。
// 所以后端必须要求**显式信号**才执行破坏性操作。

// ── 1. 心愿附件：空数组但没有 clear_attachments → 保持原绑定
func TestUpdateWishKeepsAttachmentsWhenNotExplicitlyCleared(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)
	if err := db.DB.AutoMigrate(&models.Wish{}, &models.Attachment{}, &models.Device{}); err != nil {
		t.Fatalf("迁移表失败: %v", err)
	}

	token := "lldt_test_wish_attach"
	device := models.Device{
		DeviceID: "dev_wish_attach", Name: "测试设备", Platform: "macos",
		TokenHash: middleware.HashToken(token), Status: "active",
	}
	if err := db.DB.Create(&device).Error; err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	wish := models.Wish{Title: "有图的心愿", Kind: "item", Budget: 1000}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatalf("创建心愿失败: %v", err)
	}
	att := models.Attachment{
		ObjectKey:        "entries/x.png",
		OriginalFilename: "x.png",
		MimeType:         "image/png",
		Size:             100,
		WishID:           &wish.ID,
	}
	if err := db.DB.Create(&att).Error; err != nil {
		t.Fatalf("创建附件失败: %v", err)
	}

	r := gin.New()
	r.PUT("/api/v1/wishes/:id", middleware.AdminAuthRequired(), UpdateWish)

	// 场景一：老客户端形态 —— 只发 title（其 attachment_ids 是非可选数组，必定发 []）
	body, _ := json.Marshal(map[string]any{
		"title":          "改了个标题",
		"kind":           "item",
		"attachment_ids": []int{},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/wishes/"+u64(wish.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}

	var reloaded models.Attachment
	if err := db.DB.First(&reloaded, att.ID).Error; err != nil {
		t.Fatalf("重新读取附件失败: %v", err)
	}
	if reloaded.WishID == nil || *reloaded.WishID != wish.ID {
		t.Error("空数组但未显式声明 clear_attachments 时，附件绑定**不得**被解除")
	}

	// 场景二：显式声明才真的解绑
	body2, _ := json.Marshal(map[string]any{
		"title":             "明确要清空图片",
		"kind":              "item",
		"attachment_ids":    []int{},
		"clear_attachments": true,
	})
	req2 := httptest.NewRequest(http.MethodPut, "/api/v1/wishes/"+u64(wish.ID), bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Device-Token", token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w2.Code, w2.Body.String())
	}
	var cleared models.Attachment
	if err := db.DB.First(&cleared, att.ID).Error; err != nil {
		t.Fatalf("重新读取附件失败: %v", err)
	}
	if cleared.WishID != nil {
		t.Error("显式声明 clear_attachments 时附件绑定应当被解除")
	}
}

// ── 2. 标题：显式清空后不得被「正文派生」回填
func TestUpdateEntryKeepsExplicitlyClearedTitle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)
	if err := db.DB.AutoMigrate(&models.LifeEntry{}, &models.Device{}); err != nil {
		t.Fatalf("迁移表失败: %v", err)
	}

	token := "lldt_test_title_clear"
	device := models.Device{
		DeviceID: "dev_title_clear", Name: "测试设备", Platform: "macos",
		TokenHash: middleware.HashToken(token), Status: "active",
	}
	if err := db.DB.Create(&device).Error; err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	entry := models.LifeEntry{
		Type:       "life",
		Title:      "原来的标题",
		Content:    "正文第一行\n正文第二行",
		OccurredAt: time.Now(),
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatalf("创建散页失败: %v", err)
	}

	r := gin.New()
	r.PUT("/api/v1/entries/:id", middleware.AdminAuthRequired(), UpdateLifeEntry)

	// 场景一：显式清空标题（同时带上正文）→ 标题必须保持为空，
	// 不得被「正文首行派生」回填
	body, _ := json.Marshal(map[string]any{
		"title":   "",
		"content": "正文第一行\n正文第二行",
		"type":    "life",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/entries/"+u64(entry.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}

	var reloaded models.LifeEntry
	if err := db.DB.First(&reloaded, entry.ID).Error; err != nil {
		t.Fatalf("重新读取失败: %v", err)
	}
	if reloaded.Title != "" {
		t.Errorf("显式清空标题后不得被正文派生回填，实际 %q", reloaded.Title)
	}
}

// ── 3. type 是筛选维度，空串不得把条目写成「无类型」
func TestUpdateEntryIgnoresEmptyType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)
	if err := db.DB.AutoMigrate(&models.LifeEntry{}, &models.Device{}); err != nil {
		t.Fatalf("迁移表失败: %v", err)
	}

	token := "lldt_test_type_empty"
	device := models.Device{
		DeviceID: "dev_type_empty", Name: "测试设备", Platform: "macos",
		TokenHash: middleware.HashToken(token), Status: "active",
	}
	if err := db.DB.Create(&device).Error; err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	entry := models.LifeEntry{
		Type:       "life",
		Title:      "标题",
		Content:    "正文",
		OccurredAt: time.Now(),
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatalf("创建散页失败: %v", err)
	}

	r := gin.New()
	r.PUT("/api/v1/entries/:id", middleware.AdminAuthRequired(), UpdateLifeEntry)

	body, _ := json.Marshal(map[string]any{
		"title": "改标题",
		"type":  "",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/entries/"+u64(entry.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}

	var reloaded models.LifeEntry
	if err := db.DB.First(&reloaded, entry.ID).Error; err != nil {
		t.Fatalf("重新读取失败: %v", err)
	}
	if reloaded.Type != "life" {
		t.Errorf("空 type 不得把条目写成无类型（会从所有分类视角消失），实际 %q", reloaded.Type)
	}
}

func u64(v uint) string {
	return itoa(int(v))
}
