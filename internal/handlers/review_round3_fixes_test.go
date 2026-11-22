package handlers

// 第三轮审查修复的回归测试（2026-10-25）：
// 1. RevokeDevice 末台守卫的事务内原子裁决 —— 两台并发吊销只允许成功一台，active 永不为 0；
// 2. CreateTransaction / UpdateTransaction type 枚举白名单（非法值 400，空串保持原值）；
// 3. UpdateFootprint 返程日期显式清空（空串）与解析失败 400（与 visited_at 同口径）；
// 4. UpdateFootprint 不再用城市名覆盖伴生散页的自定义地点；
// 5. UpdateWish 三个时间字段 null = 保持原值（契约 C）、空串 = 清空；
// 6. DeleteWish / DeleteFootprint 删除伴生散页后附件 entry_id 不悬挂（防永久孤儿）；
//    DeleteLifeEntry purge 解绑 Wish.footprint_id；
// 7. AddGearAsset / RestoreGearAsset 失效 /stats 缓存；
// 8. UpdateCuratorProfile 保存不回写旧 EDCGear（Omit("EDCGear")）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// ── 1. RevokeDevice：两台并发吊销必须恰好成功一台 ───────────────────

func TestRevokeDeviceConcurrentLastTwoDevices(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// WAL + busy_timeout 隔离库（与配对工单并发用例同一套）：
	// 并发写锁竞争走 busy 等待而非直接报 database is locked，裁决才有意义。
	setupPairRaceDB(t)

	now := time.Now()
	tokenA, tokenB := "lldt_race_a", "lldt_race_b"
	devA := models.Device{
		DeviceID: "dev_race_a", Name: "设备A", Platform: "web",
		TokenHash: middleware.HashToken(tokenA), Status: "active", PairedAt: now, LastActiveAt: now,
	}
	devB := models.Device{
		DeviceID: "dev_race_b", Name: "设备B", Platform: "macos",
		TokenHash: middleware.HashToken(tokenB), Status: "active", PairedAt: now, LastActiveAt: now,
	}
	if err := db.DB.Create(&devA).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&devB).Error; err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.DELETE("/api/v1/auth/devices/:id", middleware.AdminAuthRequired(), RevokeDevice)

	revoke := func(token, deviceID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/devices/"+deviceID, nil)
		req.Header.Set("X-Device-Token", token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 两台各自吊销自己的设备（最危险场景：各自都是「自己的最后一台之外的那台」）
	results := make([]*httptest.ResponseRecorder, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		results[0] = revoke(tokenA, devA.DeviceID)
	}()
	go func() {
		defer wg.Done()
		<-start
		results[1] = revoke(tokenB, devB.DeviceID)
	}()
	close(start)
	wg.Wait()

	success, rejected := 0, 0
	for _, w := range results {
		switch {
		case w.Code == http.StatusOK:
			success++
		case w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "最后一台"):
			rejected++
		default:
			t.Fatalf("并发吊销只允许 200 或 400(最后一台): %d %s", w.Code, w.Body.String())
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("仅剩两台时并发吊销必须恰好成功一台: success=%d rejected=%d", success, rejected)
	}

	var activeCount int64
	db.DB.Model(&models.Device{}).Where("status = ?", "active").Count(&activeCount)
	if activeCount != 1 {
		t.Fatalf("active 设备永不得被并发吊销清空: %d", activeCount)
	}
}

// ── 2. 记账 type 枚举白名单 ─────────────────────────────────────────

func TestTransactionTypeWhitelist(t *testing.T) {
	r, token := setupContractRouter(t)

	// 非法 type 创建 → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "transfer", "category": "餐饮",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 type 创建应 400: %d %s", w.Code, w.Body.String())
	}

	// 合法 type 创建 → 200
	w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "income", "category": "工资收入",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("合法 type 创建失败: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data models.Transaction `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/transactions/" + u64(created.Data.ID)

	// 非法 type 更新 → 400 且保持原值
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 100, "category": "工资收入", "type": "transfer",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 type 更新应 400: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Transaction
	db.DB.First(&reloaded, created.Data.ID)
	if reloaded.Type != "income" {
		t.Fatalf("被拒绝的更新不得改写 type: %s", reloaded.Type)
	}

	// type 空串 = 未携带 → 保持原值（更新语义与 MOB-P1-35 一致）
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 100, "category": "工资收入", "type": "",
	}); w.Code != http.StatusOK {
		t.Fatalf("空串 type 应视为未携带: %d %s", w.Code, w.Body.String())
	}
	db.DB.First(&reloaded, created.Data.ID)
	if reloaded.Type != "income" {
		t.Fatalf("空串 type 不得改写 type: %s", reloaded.Type)
	}
}

// ── 3. UpdateWish：null = 保持原值（契约 C），空串 = 清空 ─────────────

func TestUpdateWishNullDatesPreserveEmptyClears(t *testing.T) {
	r, token := setupContractRouter(t)

	td := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	pd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wish := models.Wish{Title: "契约C", Kind: "item", Status: "wanting", TargetDate: &td, PlantedAt: &pd}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/wishes/" + u64(wish.ID)

	// null = 保持原值（契约 C：缺省/null = 保持原值）
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"target_date": nil}); w.Code != http.StatusOK {
		t.Fatalf("target_date=null 请求失败: %d %s", w.Code, w.Body.String())
	}
	// 注意：每步断言都用**新结构体**重新查询 —— GORM 扫描 NULL 时不会把复用结构体
	// 里的非 nil 指针字段重置为 nil，复用会读到上一步的陈旧值造成假失败。
	var afterNull models.Wish
	db.DB.First(&afterNull, wish.ID)
	if afterNull.TargetDate == nil {
		t.Fatal("target_date=null 被当成清空（应保持原值）")
	}
	if afterNull.PlantedAt == nil {
		t.Fatal("未携带的 planted_at 不得受影响")
	}

	// planted_at: null 同样保持原值
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"planted_at": nil}); w.Code != http.StatusOK {
		t.Fatalf("planted_at=null 请求失败: %d %s", w.Code, w.Body.String())
	}
	var afterPlantedNull models.Wish
	db.DB.First(&afterPlantedNull, wish.ID)
	if afterPlantedNull.PlantedAt == nil {
		t.Fatal("planted_at=null 被当成清空（应保持原值）")
	}

	// 空串 = 显式清空（既有语义保持）
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"target_date": ""}); w.Code != http.StatusOK {
		t.Fatalf("target_date 空串请求失败: %d %s", w.Code, w.Body.String())
	}
	var afterClear models.Wish
	db.DB.First(&afterClear, wish.ID)
	if afterClear.TargetDate != nil {
		t.Fatalf("target_date 空串应清空: %v", afterClear.TargetDate)
	}
	if afterClear.PlantedAt == nil {
		t.Fatal("清空 target_date 不得波及 planted_at")
	}
}

// ── 4. DeleteWish：伴生散页附件不悬挂 ──────────────────────────────

func TestDeleteWishReleasesCompanionAttachments(t *testing.T) {
	r, token := setupContractRouter(t)
	r.DELETE("/api/v1/wishes/:id", middleware.AdminAuthRequired(), DeleteWish)

	wish := models.Wish{Title: "孤儿附件", Kind: "item", Status: "wanting"}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.LifeEntry{
		Type: "wish", Title: "许下心愿 · 孤儿附件", Content: "x",
		OccurredAt: time.Now(), WishID: &wish.ID,
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	onlyOwned := models.Attachment{ObjectKey: "entries/r3/wish-owned.png", EntryID: &entry.ID, WishID: &wish.ID, MimeType: "image/png"}
	if err := db.DB.Create(&onlyOwned).Error; err != nil {
		t.Fatal(err)
	}

	// purge：仅归属伴生散页的附件物理删除
	if w := doJSON(r, http.MethodDelete, "/api/v1/wishes/"+u64(wish.ID)+"?purge=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("purge 心愿失败: %d %s", w.Code, w.Body.String())
	}
	var cnt int64
	db.DB.Model(&models.Attachment{}).Where("object_key = ?", onlyOwned.ObjectKey).Count(&cnt)
	if cnt != 0 {
		t.Fatal("仅归属伴生散页的附件应随 purge 物理删除")
	}

	// 软删：附件行保留、entry_id 解绑、wish_id 保留（include_deleted 视角仍可见）
	wish2 := models.Wish{Title: "软删附件", Kind: "item", Status: "wanting"}
	if err := db.DB.Create(&wish2).Error; err != nil {
		t.Fatal(err)
	}
	entry2 := models.LifeEntry{
		Type: "wish", Title: "许下心愿 · 软删附件", Content: "x",
		OccurredAt: time.Now(), WishID: &wish2.ID,
	}
	if err := db.DB.Create(&entry2).Error; err != nil {
		t.Fatal(err)
	}
	softAtt := models.Attachment{ObjectKey: "entries/r3/wish-soft.png", EntryID: &entry2.ID, WishID: &wish2.ID, MimeType: "image/png"}
	if err := db.DB.Create(&softAtt).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodDelete, "/api/v1/wishes/"+u64(wish2.ID), token, nil); w.Code != http.StatusOK {
		t.Fatalf("软删心愿失败: %d %s", w.Code, w.Body.String())
	}
	var kept models.Attachment
	if err := db.DB.Where("object_key = ?", softAtt.ObjectKey).First(&kept).Error; err != nil {
		t.Fatal("软删不得物理删除附件行")
	}
	if kept.EntryID != nil {
		t.Fatalf("软删后 entry_id 应解绑: %v", kept.EntryID)
	}
	if kept.WishID == nil || *kept.WishID != wish2.ID {
		t.Fatal("软删应保留 wish_id 绑定")
	}
}
