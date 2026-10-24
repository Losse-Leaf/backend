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

// 跨端统一契约（A/B/C）与发现 12/15 的回归测试。
//
// 契约 B：client_request_id 幂等创建 —— 同一 id 重复提交返回已有记录，不重复落库。
// 契约 A：clear_city / clear_attachments 显式清空语义。
// 发现 15：attachment_ids 抢占其他实体的附件 → 400。
// 发现 12：已转售设备重复转售 → 400。

func setupContractRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	setupTestDB(t)
	if err := db.DB.AutoMigrate(
		&models.LifeEntry{}, &models.Transaction{}, &models.Attachment{},
		&models.Footprint{}, &models.Wish{}, &models.Device{}, &models.CuratorProfile{},
	); err != nil {
		t.Fatalf("迁移表失败: %v", err)
	}

	token := "lldt_contract_unified"
	device := models.Device{
		DeviceID: "dev_contract_unified", Name: "契约测试设备", Platform: "web",
		TokenHash: middleware.HashToken(token), Status: "active",
		// 关键：LastActiveAt 置为当前时刻，避免 AdminAuthRequired 的节流 goroutine
		// 在测试结束后仍异步写库（写的是已被下一个测试替换/清理的临时库，
		// 会与 t.TempDir 的 RemoveAll 竞态，导致偶发的 "directory not empty" 失败）。
		LastActiveAt: time.Now(),
	}
	if err := db.DB.Create(&device).Error; err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	db.DB.FirstOrCreate(&models.CuratorProfile{}, models.CuratorProfile{ID: 1})

	r := gin.New()
	r.GET("/api/v1/entries/:id", GetLifeEntryByID)
	r.POST("/api/v1/entries", middleware.AdminAuthRequired(), CreateLifeEntry)
	r.PUT("/api/v1/entries/:id", middleware.AdminAuthRequired(), UpdateLifeEntry)
	r.POST("/api/v1/transactions", middleware.AdminAuthRequired(), CreateTransaction)
	r.PUT("/api/v1/transactions/:id", middleware.AdminAuthRequired(), UpdateTransaction)
	r.POST("/api/v1/assets/gear", middleware.AdminAuthRequired(), AddGearAsset)
	r.POST("/api/v1/assets/gear/:id/sell", middleware.AdminAuthRequired(), SellGearAsset)
	return r, token
}

func doJSON(r *gin.Engine, method, path, token string, body map[string]any) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ── 契约 B：同一 client_request_id 重复提交散页 → 返回同一条记录
func TestCreateEntryIdempotentByClientRequestID(t *testing.T) {
	r, token := setupContractRouter(t)

	body := map[string]any{
		"type": "life", "content": "幂等创建的散页",
		"client_request_id": "crid-entry-0001",
	}
	w1 := doJSON(r, http.MethodPost, "/api/v1/entries", token, body)
	if w1.Code != http.StatusOK {
		t.Fatalf("首次创建失败: %d %s", w1.Code, w1.Body.String())
	}
	var first struct {
		Data models.LifeEntry `json:"data"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &first)

	w2 := doJSON(r, http.MethodPost, "/api/v1/entries", token, body)
	if w2.Code != http.StatusOK {
		t.Fatalf("重复提交应幂等返回 200: %d %s", w2.Code, w2.Body.String())
	}
	var second struct {
		Data models.LifeEntry `json:"data"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &second)

	if first.Data.ID != second.Data.ID {
		t.Fatalf("同一 client_request_id 应返回同一条记录: 首次=%d, 重试=%d", first.Data.ID, second.Data.ID)
	}

	var count int64
	db.DB.Model(&models.LifeEntry{}).Where("client_request_id = ?", "crid-entry-0001").Count(&count)
	if count != 1 {
		t.Fatalf("幂等创建不得重复落库: 实际 %d 条", count)
	}
}

// ── 契约 B：同一 client_request_id 重复记账 → 只有一笔
func TestCreateTransactionIdempotentByClientRequestID(t *testing.T) {
	r, token := setupContractRouter(t)

	body := map[string]any{
		"amount": 8800, "type": "expense", "category": "餐饮", "title": "幂等记账",
		"client_request_id": "crid-tx-0001",
	}
	w1 := doJSON(r, http.MethodPost, "/api/v1/transactions", token, body)
	if w1.Code != http.StatusOK {
		t.Fatalf("首次记账失败: %d %s", w1.Code, w1.Body.String())
	}
	w2 := doJSON(r, http.MethodPost, "/api/v1/transactions", token, body)
	if w2.Code != http.StatusOK {
		t.Fatalf("重复提交应幂等返回 200: %d %s", w2.Code, w2.Body.String())
	}

	var count int64
	db.DB.Model(&models.Transaction{}).Where("client_request_id = ?", "crid-tx-0001").Count(&count)
	if count != 1 {
		t.Fatalf("幂等记账不得重复落库: 实际 %d 笔", count)
	}
}

// ── 契约 A：clear_city 显式清空散页属地，且优先于 city_code 字段
func TestUpdateEntryClearCity(t *testing.T) {
	r, token := setupContractRouter(t)

	entry := models.LifeEntry{Type: "travel", Title: "属地清空", Content: "x", CityCode: "330100", CityName: "杭州市"}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatalf("创建散页失败: %v", err)
	}

	// 携带 city_code 也必须被 clear_city 压过（优先级契约）
	w := doJSON(r, http.MethodPut, "/api/v1/entries/"+u64(entry.ID), token, map[string]any{
		"clear_city": true, "city_code": "110100",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.LifeEntry
	if err := db.DB.First(&reloaded, entry.ID).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if reloaded.CityCode != "" {
		t.Fatalf("clear_city 后 city_code 应为空, 实际 %q", reloaded.CityCode)
	}
}

// ── 契约 A：UpdateTransaction 的 clear_city
func TestUpdateTransactionClearCity(t *testing.T) {
	r, token := setupContractRouter(t)

	tx := models.Transaction{Amount: 100, Type: "expense", Category: "餐饮", CityCode: "330100"}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatalf("创建账目失败: %v", err)
	}

	w := doJSON(r, http.MethodPut, "/api/v1/transactions/"+u64(tx.ID), token, map[string]any{
		"amount": 100, "category": "餐饮", "clear_city": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Transaction
	if err := db.DB.First(&reloaded, tx.ID).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if reloaded.CityCode != "" {
		t.Fatalf("clear_city 后账目 city_code 应为空, 实际 %q", reloaded.CityCode)
	}
}

// ── 契约 A：UpdateEntry clear_attachments 先解绑、再绑定新列表
func TestUpdateEntryClearAttachmentsThenRebind(t *testing.T) {
	r, token := setupContractRouter(t)

	entry := models.LifeEntry{Type: "life", Title: "附件解绑", Content: "x"}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatalf("创建散页失败: %v", err)
	}
	old1 := models.Attachment{ObjectKey: "entries/old1.png", MimeType: "image/png", EntryID: &entry.ID}
	old2 := models.Attachment{ObjectKey: "entries/old2.png", MimeType: "image/png", EntryID: &entry.ID}
	db.DB.Create(&old1)
	db.DB.Create(&old2)
	fresh := models.Attachment{ObjectKey: "entries/fresh.png", MimeType: "image/png"}
	db.DB.Create(&fresh)

	w := doJSON(r, http.MethodPut, "/api/v1/entries/"+u64(entry.ID), token, map[string]any{
		"clear_attachments": true,
		"attachment_ids":    []uint{fresh.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}

	for _, att := range []models.Attachment{old1, old2} {
		var reloaded models.Attachment
		if err := db.DB.First(&reloaded, att.ID).Error; err != nil {
			t.Fatalf("回读失败: %v", err)
		}
		if reloaded.EntryID != nil {
			t.Errorf("clear_attachments 后旧附件 #%d 不得再绑定本散页", att.ID)
		}
	}
	var rebound models.Attachment
	if err := db.DB.First(&rebound, fresh.ID).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if rebound.EntryID == nil || *rebound.EntryID != entry.ID {
		t.Error("clear_attachments + attachment_ids 应绑定新列表")
	}
}

// ── 发现 15：attachment_ids 抢占其他实体的附件 → 400
func TestUpdateEntryRejectsClaimingForeignAttachment(t *testing.T) {
	r, token := setupContractRouter(t)

	mine := models.LifeEntry{Type: "life", Title: "我的散页", Content: "x"}
	db.DB.Create(&mine)
	other := models.LifeEntry{Type: "life", Title: "别人的散页", Content: "y"}
	db.DB.Create(&other)
	foreign := models.Attachment{ObjectKey: "entries/foreign.png", MimeType: "image/png", EntryID: &other.ID}
	db.DB.Create(&foreign)

	w := doJSON(r, http.MethodPut, "/api/v1/entries/"+u64(mine.ID), token, map[string]any{
		"attachment_ids": []uint{foreign.ID},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("抢占他人附件应 400, 实际 %d %s", w.Code, w.Body.String())
	}

	var reloaded models.Attachment
	db.DB.First(&reloaded, foreign.ID)
	if reloaded.EntryID == nil || *reloaded.EntryID != other.ID {
		t.Error("被拒绝的绑定不得改写原归属")
	}
}

// ── 契约 C：UpdateTransaction 缺省 title/note → 保持原值
func TestUpdateTransactionOmittedTextFieldsKeepValues(t *testing.T) {
	r, token := setupContractRouter(t)

	tx := models.Transaction{Amount: 100, Type: "expense", Category: "餐饮", Title: "原标题", Note: "原备注", PaymentMethod: "现金"}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatalf("创建账目失败: %v", err)
	}

	// 只改金额；不带 title/note/payment_method（契约 C：缺省 = 保持原值）
	w := doJSON(r, http.MethodPut, "/api/v1/transactions/"+u64(tx.ID), token, map[string]any{
		"amount": 200, "category": "餐饮",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Transaction
	if err := db.DB.First(&reloaded, tx.ID).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if reloaded.Title != "原标题" || reloaded.Note != "原备注" || reloaded.PaymentMethod != "现金" {
		t.Fatalf("缺省字段应保持原值: title=%q note=%q pay=%q", reloaded.Title, reloaded.Note, reloaded.PaymentMethod)
	}

	// 显式空串 = 清空
	w2 := doJSON(r, http.MethodPut, "/api/v1/transactions/"+u64(tx.ID), token, map[string]any{
		"amount": 200, "category": "餐饮", "note": "",
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w2.Code, w2.Body.String())
	}
	var cleared models.Transaction
	db.DB.First(&cleared, tx.ID)
	if cleared.Note != "" {
		t.Fatalf("空串应清空 note, 实际 %q", cleared.Note)
	}
}

// ── 发现 12：已转售设备重复转售 → 400
func TestSellGearAssetRejectsAlreadySold(t *testing.T) {
	r, token := setupContractRouter(t)

	w := doJSON(r, http.MethodPost, "/api/v1/assets/gear", token, map[string]any{
		"name": "测试相机", "category": "数码器材", "price_cents": 100000,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("录入设备失败: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data models.GearItem `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	sell := func() *httptest.ResponseRecorder {
		return doJSON(r, http.MethodPost, "/api/v1/assets/gear/"+created.Data.ID+"/sell", token, map[string]any{
			"sold_price_cents": 50000,
		})
	}
	if w := sell(); w.Code != http.StatusOK {
		t.Fatalf("首次转售失败: %d %s", w.Code, w.Body.String())
	}
	if w := sell(); w.Code != http.StatusBadRequest {
		t.Fatalf("重复转售应 400, 实际 %d %s", w.Code, w.Body.String())
	}

	// 只应产生一笔回血收入
	var income int64
	db.DB.Model(&models.Transaction{}).
		Where("type = 'income' AND title = ?", "设备转售回血: 测试相机").
		Select("COALESCE(COUNT(*), 0)").Scan(&income)
	if income != 1 {
		t.Fatalf("重复转售不得重复生成回血收入, 实际 %d 笔", income)
	}
}

// ── 发现 5：公开详情接口非数字 id → 400
func TestPublicDetailEndpointsRejectNonNumericID(t *testing.T) {
	r, token := setupContractRouter(t)
	_ = token

	// 注入形态（URL 编码后的 "2 OR 1=1"）：必须被入口拦截为 400
	req := httptest.NewRequest(http.MethodGet, "/api/v1/entries/2%20OR%201=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非数字 id 应 400, 实际 %d", w.Code)
	}
}
