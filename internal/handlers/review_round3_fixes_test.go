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

// ── 3. UpdateFootprint：返程日期显式清空与非法值 400 ─────────────────

func TestUpdateFootprintDepartedAtClearAndInvalid(t *testing.T) {
	r, token := setupContractRouter(t)

	dep := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	fp := models.Footprint{
		CityCode: "330100", CityName: "杭州市",
		VisitedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), DepartedAt: &dep,
	}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/footprints/" + u64(fp.ID)

	// 空串 = 显式清空返程（Web 编辑弹窗的既定契约：后端 hasKey+空串=清空）
	w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"city_code": "330100", "city_name": "杭州市",
		"visited_at":  "2026-05-01T00:00:00+08:00",
		"departed_at": "",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("清空返程失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Footprint
	db.DB.First(&reloaded, fp.ID)
	if reloaded.DepartedAt != nil {
		t.Fatalf("departed_at 空串应清空返程日期, 仍为 %v", reloaded.DepartedAt)
	}

	// 非法值 → 400（与 visited_at 同口径，不再静默忽略）
	w2 := doJSON(r, http.MethodPut, path, token, map[string]any{
		"city_code": "330100", "city_name": "杭州市",
		"departed_at": "not-a-date",
	})
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("非法 departed_at 应 400: %d %s", w2.Code, w2.Body.String())
	}
}

// ── 4. UpdateFootprint：城市名不再覆盖伴生散页的自定义地点 ───────────

func TestUpdateFootprintKeepsEntryLocation(t *testing.T) {
	r, token := setupContractRouter(t)

	entry := models.LifeEntry{
		Type: "travel", Title: "杭州行", Content: "x",
		OccurredAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		CityCode:   "330100", CityName: "杭州市", Location: "西湖边的小咖啡馆",
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	fp := models.Footprint{
		CityCode: "330100", CityName: "杭州市",
		VisitedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), LifeEntryID: &entry.ID,
	}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/footprints/" + u64(fp.ID)

	// 只改标题：自定义地点必须原样保留
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"title": "杭州春行"}); w.Code != http.StatusOK {
		t.Fatalf("更新标题失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.LifeEntry
	db.DB.First(&reloaded, entry.ID)
	if reloaded.Location != "西湖边的小咖啡馆" {
		t.Fatalf("未改城市时不得覆盖散页地点: %q", reloaded.Location)
	}

	// 改城市：散页城市字段跟随新城市，自定义地点仍保留
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"city_code": "320100", "city_name": "南京市",
	}); w.Code != http.StatusOK {
		t.Fatalf("改城市失败: %d %s", w.Code, w.Body.String())
	}
	db.DB.First(&reloaded, entry.ID)
	if reloaded.CityCode != "320100" || reloaded.CityName != "南京市" {
		t.Fatalf("改城市后散页城市字段应同步: %s/%s", reloaded.CityCode, reloaded.CityName)
	}
	if reloaded.Location != "西湖边的小咖啡馆" {
		t.Fatalf("改城市也不得覆盖自定义地点: %q", reloaded.Location)
	}

	// 地点原本就是自动回填的城市名时，随城市改名而跟随
	entry2 := models.LifeEntry{
		Type: "travel", Title: "南京行", Content: "x",
		OccurredAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		CityCode:   "320100", CityName: "南京市", Location: "南京市",
	}
	if err := db.DB.Create(&entry2).Error; err != nil {
		t.Fatal(err)
	}
	fp2 := models.Footprint{
		CityCode: "320100", CityName: "南京市",
		VisitedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), LifeEntryID: &entry2.ID,
	}
	if err := db.DB.Create(&fp2).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodPut, "/api/v1/footprints/"+u64(fp2.ID), token, map[string]any{
		"city_code": "440100", "city_name": "广州市",
	}); w.Code != http.StatusOK {
		t.Fatalf("改城市失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded2 models.LifeEntry
	db.DB.First(&reloaded2, entry2.ID)
	if reloaded2.Location != "广州市" {
		t.Fatalf("自动回填的城市名地点应随城市改名跟随: %q", reloaded2.Location)
	}
}

// ── 5. UpdateWish：null = 保持原值（契约 C），空串 = 清空 ─────────────

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

// ── 6a. DeleteWish：伴生散页附件不悬挂 ──────────────────────────────

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
	fp := models.Footprint{CityCode: "330100", CityName: "杭州市", VisitedAt: time.Now()}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatal(err)
	}
	onlyOwned := models.Attachment{ObjectKey: "entries/r3/wish-owned.png", EntryID: &entry.ID, WishID: &wish.ID, MimeType: "image/png"}
	shared := models.Attachment{ObjectKey: "entries/r3/wish-shared.png", EntryID: &entry.ID, WishID: &wish.ID, FootprintID: &fp.ID, MimeType: "image/png"}
	if err := db.DB.Create(&onlyOwned).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&shared).Error; err != nil {
		t.Fatal(err)
	}

	// purge：仅归属伴生散页的附件物理删除，共享附件只解绑 entry_id
	if w := doJSON(r, http.MethodDelete, "/api/v1/wishes/"+u64(wish.ID)+"?purge=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("purge 心愿失败: %d %s", w.Code, w.Body.String())
	}
	var cnt int64
	db.DB.Model(&models.Attachment{}).Where("object_key = ?", onlyOwned.ObjectKey).Count(&cnt)
	if cnt != 0 {
		t.Fatal("仅归属伴生散页的附件应随 purge 物理删除")
	}
	var sharedAtt models.Attachment
	if err := db.DB.Where("object_key = ?", shared.ObjectKey).First(&sharedAtt).Error; err != nil {
		t.Fatal("共享附件的行不得删除")
	}
	if sharedAtt.EntryID != nil {
		t.Fatalf("共享附件的 entry_id 应解绑: %v", sharedAtt.EntryID)
	}
	if sharedAtt.WishID != nil {
		t.Fatal("共享附件的 wish_id 应解绑")
	}
	if sharedAtt.FootprintID == nil || *sharedAtt.FootprintID != fp.ID {
		t.Fatal("共享附件的 footprint_id 不得受影响")
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

// ── 6b. DeleteFootprint：首生散页附件不悬挂 ─────────────────────────

func TestDeleteFootprintReleasesCompanionAttachments(t *testing.T) {
	r, token := setupContractRouter(t)
	r.DELETE("/api/v1/footprints/:id", middleware.AdminAuthRequired(), DeleteFootprint)

	wish := models.Wish{Title: "共享心愿", Kind: "item", Status: "wanting"}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.LifeEntry{
		Type: "travel", Title: "杭州行", Content: "x",
		OccurredAt: time.Now(), CityCode: "330100", CityName: "杭州市",
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	fp := models.Footprint{
		CityCode: "330100", CityName: "杭州市",
		VisitedAt: time.Now(), LifeEntryID: &entry.ID,
	}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatal(err)
	}
	// 仅归属「本旅程+首生散页」的附件（purge 时应物理删除）
	owned := models.Attachment{ObjectKey: "entries/r3/fp-owned.png", EntryID: &entry.ID, FootprintID: &fp.ID, MimeType: "image/png"}
	// 与心愿共享的附件（purge 时只解绑 entry_id，保留在心愿上）
	shared := models.Attachment{ObjectKey: "entries/r3/fp-shared.png", EntryID: &entry.ID, FootprintID: &fp.ID, WishID: &wish.ID, MimeType: "image/png"}
	if err := db.DB.Create(&owned).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&shared).Error; err != nil {
		t.Fatal(err)
	}

	if w := doJSON(r, http.MethodDelete, "/api/v1/footprints/"+u64(fp.ID)+"?purge=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("purge 足迹失败: %d %s", w.Code, w.Body.String())
	}
	var cnt int64
	db.DB.Model(&models.Attachment{}).Where("object_key = ?", owned.ObjectKey).Count(&cnt)
	if cnt != 0 {
		t.Fatal("仅归属本旅程+首生散页的附件应随 purge 物理删除")
	}
	var sharedAtt models.Attachment
	if err := db.DB.Where("object_key = ?", shared.ObjectKey).First(&sharedAtt).Error; err != nil {
		t.Fatal("共享附件的行不得删除")
	}
	if sharedAtt.EntryID != nil || sharedAtt.FootprintID != nil {
		t.Fatalf("共享附件的 entry_id/footprint_id 应解绑: %v/%v", sharedAtt.EntryID, sharedAtt.FootprintID)
	}
	if sharedAtt.WishID == nil || *sharedAtt.WishID != wish.ID {
		t.Fatal("共享附件的 wish_id 不得受影响")
	}

	// 软删：附件行保留、entry_id 解绑、footprint_id 保留（include_deleted 视角仍可见）
	entry2 := models.LifeEntry{
		Type: "travel", Title: "南京行", Content: "x",
		OccurredAt: time.Now(), CityCode: "320100", CityName: "南京市",
	}
	if err := db.DB.Create(&entry2).Error; err != nil {
		t.Fatal(err)
	}
	fp2 := models.Footprint{
		CityCode: "320100", CityName: "南京市",
		VisitedAt: time.Now(), LifeEntryID: &entry2.ID,
	}
	if err := db.DB.Create(&fp2).Error; err != nil {
		t.Fatal(err)
	}
	softAtt := models.Attachment{ObjectKey: "entries/r3/fp-soft.png", EntryID: &entry2.ID, FootprintID: &fp2.ID, MimeType: "image/png"}
	if err := db.DB.Create(&softAtt).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodDelete, "/api/v1/footprints/"+u64(fp2.ID), token, nil); w.Code != http.StatusOK {
		t.Fatalf("软删足迹失败: %d %s", w.Code, w.Body.String())
	}
	var kept models.Attachment
	if err := db.DB.Where("object_key = ?", softAtt.ObjectKey).First(&kept).Error; err != nil {
		t.Fatal("软删不得物理删除附件行")
	}
	if kept.EntryID != nil {
		t.Fatalf("软删后 entry_id 应解绑: %v", kept.EntryID)
	}
	if kept.FootprintID == nil || *kept.FootprintID != fp2.ID {
		t.Fatal("软删应保留 footprint_id 绑定")
	}
}

// ── 6c. DeleteLifeEntry purge：解绑 Wish.footprint_id ───────────────

func TestDeleteLifeEntryPurgeUnbindsWishFootprint(t *testing.T) {
	r, token := setupContractRouter(t)
	r.DELETE("/api/v1/entries/:id", middleware.AdminAuthRequired(), DeleteLifeEntry)

	entry := models.LifeEntry{
		Type: "travel", Title: "杭州行", Content: "x",
		OccurredAt: time.Now(), CityCode: "330100", CityName: "杭州市",
	}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	fp := models.Footprint{
		CityCode: "330100", CityName: "杭州市",
		VisitedAt: time.Now(), LifeEntryID: &entry.ID,
	}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatal(err)
	}
	wish := models.Wish{Title: "杭州心愿", Kind: "travel", Status: "wanting", FootprintID: &fp.ID}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}

	if w := doJSON(r, http.MethodDelete, "/api/v1/entries/"+u64(entry.ID)+"?purge=true", token, nil); w.Code != http.StatusOK {
		t.Fatalf("purge 散页失败: %d %s", w.Code, w.Body.String())
	}

	var fpCount int64
	db.DB.Unscoped().Model(&models.Footprint{}).Where("id = ?", fp.ID).Count(&fpCount)
	if fpCount != 0 {
		t.Fatal("无其他条目引用的首生足迹应物理删除")
	}
	var reloaded models.Wish
	db.DB.First(&reloaded, wish.ID)
	if reloaded.FootprintID != nil {
		t.Fatalf("Wish.footprint_id 应解绑: %v", reloaded.FootprintID)
	}
}

// ── 7. AddGearAsset / RestoreGearAsset 失效 /stats 缓存 ─────────────

func TestGearWritePathsInvalidateStatsCache(t *testing.T) {
	r, token := setupContractRouter(t)
	r.DELETE("/api/v1/assets/gear/:id", middleware.AdminAuthRequired(), DeleteGearAsset)
	r.POST("/api/v1/assets/gear/restore", middleware.AdminAuthRequired(), RestoreGearAsset)

	warmStatsCache := func() {
		statsCacheMutex.Lock()
		cachedStats = &statsSnapshot{TotalEntries: 1, TotalPhotos: 1, CurrentYear: 2026, CurrentMonth: 10}
		cachedStatsTime = time.Now()
		statsCacheMutex.Unlock()
	}
	cacheInvalidated := func() bool {
		statsCacheMutex.RLock()
		defer statsCacheMutex.RUnlock()
		return cachedStats == nil
	}

	// AddGearAsset（联动记账）必须失效缓存
	warmStatsCache()
	w := doJSON(r, http.MethodPost, "/api/v1/assets/gear", token, map[string]any{
		"name": "缓存测试设备", "price_cents": 10000, "create_transaction": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("录入设备失败: %d %s", w.Code, w.Body.String())
	}
	if !cacheInvalidated() {
		t.Fatal("AddGearAsset 后 /stats 缓存应失效")
	}
	var created struct {
		Data models.GearItem `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// RestoreGearAsset 必须失效缓存
	warmStatsCache()
	if w := doJSON(r, http.MethodDelete, "/api/v1/assets/gear/"+created.Data.ID, token, nil); w.Code != http.StatusOK {
		t.Fatalf("删除设备失败: %d %s", w.Code, w.Body.String())
	}
	warmStatsCache()
	if w := doJSON(r, http.MethodPost, "/api/v1/assets/gear/restore", token, map[string]any{
		"id": created.Data.ID, "name": created.Data.Name,
	}); w.Code != http.StatusOK {
		t.Fatalf("恢复设备失败: %d %s", w.Code, w.Body.String())
	}
	if !cacheInvalidated() {
		t.Fatal("RestoreGearAsset 后 /stats 缓存应失效")
	}
}

// ── 8. UpdateCuratorProfile：保存不回写旧 EDCGear ───────────────────

func TestUpdateCuratorProfileKeepsEDCGear(t *testing.T) {
	r, token := setupContractRouter(t)
	r.PUT("/api/v1/profile", middleware.AdminAuthRequired(), UpdateCuratorProfile)

	db.DB.Model(&models.CuratorProfile{}).Where("id = ?", 1).
		Update("edc_gear", `[{"id":"gear_a","name":"旧设备","category":"数码器材","status":"active"}]`)

	// 复现处理器的「读-改-写」竞态窗口：
	// 处理器在请求开始时读出档案（stale）→ 并发器物写入新增 gear_b → 处理器再保存。
	// 保存步必须与 UpdateCuratorProfile 内一致（Omit("EDCGear") 的整行保存）：
	// 若回退成裸 Save，这里会拿旧 EDCGear 覆盖掉并发新增的设备。
	var stale models.CuratorProfile
	if err := db.DB.First(&stale, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := mutateProfileGear(func(list []models.GearItem) ([]models.GearItem, error) {
		return append(list, models.GearItem{ID: "gear_b", Name: "并发新设备", Category: "数码器材", Status: "active"}), nil
	}); err != nil {
		t.Fatal(err)
	}
	stale.UpdatedAt = time.Now()
	if err := db.DB.Omit("EDCGear").Save(&stale).Error; err != nil {
		t.Fatal(err)
	}
	var reloaded models.CuratorProfile
	db.DB.First(&reloaded, 1)
	if !strings.Contains(reloaded.EDCGear, "gear_b") {
		t.Fatalf("档案保存不得回写旧 EDCGear 冲掉并发新增的设备: %s", reloaded.EDCGear)
	}

	// 端到端：PUT /profile 更新 motto，器物清单不受影响
	if w := doJSON(r, http.MethodPut, "/api/v1/profile", token, map[string]any{"motto": "新座右铭"}); w.Code != http.StatusOK {
		t.Fatalf("更新档案失败: %d %s", w.Code, w.Body.String())
	}
	db.DB.First(&reloaded, 1)
	if reloaded.Motto != "新座右铭" {
		t.Fatalf("motto 应更新: %q", reloaded.Motto)
	}
	if !strings.Contains(reloaded.EDCGear, "gear_b") {
		t.Fatalf("PUT /profile 不得影响器物清单: %s", reloaded.EDCGear)
	}
}
