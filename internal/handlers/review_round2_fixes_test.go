package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// 二轮审查修复的回归测试：
// 1. RevokeDevice 拒绝吊销最后一台 active 设备（防永久锁死）；
// 2. ConfirmPairing 配对工单事务内条件核销（防双花）；
// 3. LifeEntry type / Wish kind/status 枚举白名单（非法值 400）；
// 4. UpdateTransaction 显式 footprint_id=0 解绑后不被自动重挂。

// ── 1. 最后一台 active 设备拒绝吊销 ───────────────────────────────

func TestRevokeDeviceRejectsLastActiveDevice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)

	r := gin.New()
	r.DELETE("/api/v1/auth/devices/:id", RevokeDevice)

	now := time.Now()
	first := models.Device{
		DeviceID: "dev_round2_first", Name: "第一台", Platform: "macos",
		TokenHash: "hash_first", Status: "active", PairedAt: now, LastActiveAt: now,
	}
	second := models.Device{
		DeviceID: "dev_round2_second", Name: "第二台", Platform: "web",
		TokenHash: "hash_second", Status: "active", PairedAt: now, LastActiveAt: now,
	}
	if err := db.DB.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&second).Error; err != nil {
		t.Fatal(err)
	}

	// 非最后一台 active：吊销放行（包括吊销当前请求所用设备的场景）
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/devices/"+first.DeviceID, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("吊销非最后一台应放行: %d %s", w.Code, w.Body.String())
	}

	// 剩下的最后一台 active：400 且提示先配对新设备
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/auth/devices/"+second.DeviceID, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("吊销最后一台 active 设备应 400: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "请先配对一台新设备") {
		t.Fatalf("错误信息应提示先配对新设备: %s", w.Body.String())
	}

	var device models.Device
	db.DB.Where("device_id = ?", second.DeviceID).First(&device)
	if device.Status != "active" {
		t.Fatalf("被拒绝的吊销不得改写设备状态: %s", device.Status)
	}
}

// ── 2. 配对工单条件核销（防双花） ─────────────────────────────────

// setupPairRaceDB 并发用例专用库：带 busy_timeout 的 SQLite，
// 避免写锁竞争被驱动直接判为 database is locked 而非走核销裁决。
func setupPairRaceDB(t *testing.T) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pair_race.db")
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	database, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	_ = database.AutoMigrate(&models.SiteConfig{}, &models.CuratorProfile{}, &models.Device{}, &models.PairTicket{})
	database.Create(&models.SiteConfig{Initialized: true})
	db.DB = database
}

func postConfirmPair(r *gin.Engine, ticket, forwardedFor string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(ConfirmPairingRequest{
		Ticket:     ticket,
		DeviceName: "并发握手设备",
		Platform:   "web",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if forwardedFor != "" {
		// 隔离 pairLimiter 的按 IP 计数窗口
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestConfirmPairingUsedTicketRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPairRaceDB(t)

	r := gin.New()
	r.POST("/api/v1/auth/pair/confirm", ConfirmPairing)

	db.DB.Create(&models.PairTicket{
		Ticket: "pair_tk_used_seq", ExpiresAt: time.Now().Add(5 * time.Minute), Used: true,
	})
	w := postConfirmPair(r, "pair_tk_used_seq", "")
	// 预检（工单已是 used）或事务内核销裁决都会以 400 拒绝，二者提示语均含「已被核销」
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "已被核销") {
		t.Fatalf("已核销工单应 400 并提示已被核销: %d %s", w.Code, w.Body.String())
	}

	var deviceCount int64
	db.DB.Model(&models.Device{}).Count(&deviceCount)
	if deviceCount != 0 {
		t.Fatalf("被拒绝的握手不得创建设备: %d", deviceCount)
	}
}

func TestConfirmPairingSingleRedemptionUnderConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPairRaceDB(t)

	r := gin.New()
	r.POST("/api/v1/auth/pair/confirm", ConfirmPairing)

	db.DB.Create(&models.PairTicket{
		Ticket: "pair_tk_race", ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	const n = 6
	results := make([]*httptest.ResponseRecorder, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = postConfirmPair(r, "pair_tk_race", fmt.Sprintf("10.20.0.%d", i+1))
		}(i)
	}
	close(start)
	wg.Wait()

	success, redeemed := 0, 0
	for _, w := range results {
		switch {
		case w.Code == http.StatusOK:
			success++
		case w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "已被核销"):
			// 预检（读到 used=1）与事务内条件核销裁决（RowsAffected=0）都算被拒绝
			redeemed++
		default:
			t.Fatalf("并发握手只允许 200 或 400(已被核销): %d %s", w.Code, w.Body.String())
		}
	}
	if success != 1 || redeemed != n-1 {
		t.Fatalf("同一工单只能核销一次: success=%d redeemed=%d", success, redeemed)
	}

	var deviceCount int64
	db.DB.Model(&models.Device{}).Count(&deviceCount)
	if deviceCount != 1 {
		t.Fatalf("工单双花不得创建多台设备: %d", deviceCount)
	}
	var ticket models.PairTicket
	db.DB.Where("ticket = ?", "pair_tk_race").First(&ticket)
	if !ticket.Used || ticket.PairedDeviceID == "" {
		t.Fatalf("工单应登记核销设备: %+v", ticket)
	}
}

// ── 3. type / kind / status 枚举白名单 ────────────────────────────

func TestLifeEntryTypeWhitelist(t *testing.T) {
	r, token := setupContractRouter(t)

	// 非法 type 创建 → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/entries", token, map[string]any{
		"type": "photo", "content": "非法类型",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 type 创建应 400: %d %s", w.Code, w.Body.String())
	}

	// 合法 type 创建 → 200
	w := doJSON(r, http.MethodPost, "/api/v1/entries", token, map[string]any{
		"type": "travel", "content": "合法类型",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("合法 type 创建失败: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data models.LifeEntry `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/entries/" + u64(created.Data.ID)

	// 非法 type 更新 → 400 且保持原值
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"type": "thought"}); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 type 更新应 400: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.LifeEntry
	db.DB.First(&reloaded, created.Data.ID)
	if reloaded.Type != "travel" {
		t.Fatalf("被拒绝的更新不得改写 type: %s", reloaded.Type)
	}

	// type 空串 = 未携带 → 保持原值
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"type": ""}); w.Code != http.StatusOK {
		t.Fatalf("空串 type 应视为未携带: %d %s", w.Code, w.Body.String())
	}
	db.DB.First(&reloaded, created.Data.ID)
	if reloaded.Type != "travel" {
		t.Fatalf("空串 type 不得清空: %s", reloaded.Type)
	}
}

func TestWishKindStatusWhitelist(t *testing.T) {
	r, token := setupContractRouter(t)
	r.POST("/api/v1/wishes", middleware.AdminAuthRequired(), CreateWish)

	// 非法 kind 创建 → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/wishes", token, map[string]any{
		"title": "心愿", "kind": "experience",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 kind 创建应 400: %d %s", w.Code, w.Body.String())
	}

	// 合法 kind 创建 → 200
	w := doJSON(r, http.MethodPost, "/api/v1/wishes", token, map[string]any{
		"title": "去杭州看展", "kind": "travel",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("合法 kind 创建失败: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data models.Wish `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/wishes/" + u64(created.Data.ID)

	// 非法 kind / status 更新 → 400
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"kind": "emotion"}); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 kind 更新应 400: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"status": "done"}); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 status 更新应 400: %d %s", w.Code, w.Body.String())
	}

	// 合法 status 更新 → 200 且落库
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"status": "shelved"}); w.Code != http.StatusOK {
		t.Fatalf("合法 status 更新失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Wish
	db.DB.First(&reloaded, created.Data.ID)
	if reloaded.Kind != "travel" || reloaded.Status != "shelved" {
		t.Fatalf("白名单值应正常落库: kind=%s status=%s", reloaded.Kind, reloaded.Status)
	}
}

// ── 4. 显式解绑旅程后不得被自动重挂 ────────────────────────────────

func TestUpdateTransactionFootprintZeroUnbindSkipsAutoRelink(t *testing.T) {
	r, token := setupContractRouter(t)

	fp := models.Footprint{CityCode: "330100", CityName: "杭州市", VisitedAt: time.Now()}
	if err := db.DB.Create(&fp).Error; err != nil {
		t.Fatal(err)
	}
	tx := models.Transaction{
		Amount: 100, Type: "expense", Category: "餐饮",
		CityCode: "330100", OccurredAt: time.Now(),
	}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatal(err)
	}
	// 模拟已被挂载到旅程的状态
	if err := db.DB.Model(&tx).Update("footprint_id", fp.ID).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/transactions/" + u64(tx.ID)

	// 显式携带 footprint_id=0 = 解绑：同城窗口规则不得立刻把它挂回去
	w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 100, "category": "餐饮", "footprint_id": 0,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("解绑失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Transaction
	db.DB.First(&reloaded, tx.ID)
	if reloaded.FootprintID != nil {
		t.Fatalf("显式解绑后不得被自动重挂: %v", reloaded.FootprintID)
	}

	// 未携带 footprint_id（nil）→ 行为不变：自动对齐仍会挂回同城窗口内的旅程
	w2 := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 100, "category": "餐饮",
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w2.Code, w2.Body.String())
	}
	db.DB.First(&reloaded, tx.ID)
	if reloaded.FootprintID == nil || *reloaded.FootprintID != fp.ID {
		t.Fatalf("未携带 footprint_id 时自动挂载行为应保持: %v", reloaded.FootprintID)
	}
}
