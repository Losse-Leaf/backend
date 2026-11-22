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

// 跨端统一契约（A/B/C）与发现 15 的回归测试。
//
// 契约 B：client_request_id 幂等创建 —— 同一 id 重复提交返回已有记录，不重复落库。
// 契约 A：clear_attachments 显式清空语义。
// 发现 15：attachment_ids 抢占其他实体的附件 → 400。

func setupContractRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	setupTestDB(t)
	if err := db.DB.AutoMigrate(
		&models.LifeEntry{}, &models.Transaction{}, &models.Attachment{},
		&models.Wish{}, &models.Device{}, &models.CuratorProfile{},
		&models.CreditChannel{}, &models.InstallmentPlan{}, &models.InstallmentItem{},
		&models.SyncTombstone{},
	); err != nil {
		t.Fatalf("迁移表失败: %v", err)
	}

	token := "lldt_contract_unified"
	device := models.Device{
		DeviceID: "dev_contract_unified", Name: "契约测试设备", Platform: "web",
		TokenHash: middleware.HashToken(token), Status: "active",
		// 关键：LastActiveAt 置为当前时刻，跳过 AdminAuthRequired 的活跃时间节流更新，
		// 避免测试请求额外写入本用例的临时库。
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
	r.PUT("/api/v1/wishes/:id", middleware.AdminAuthRequired(), UpdateWish)
	r.POST("/api/v1/wishes", middleware.AdminAuthRequired(), CreateWish)
	r.POST("/api/v1/wishes/:id/resolve", middleware.AdminAuthRequired(), ResolveWish)
	r.POST("/api/v1/transactions", middleware.AdminAuthRequired(), CreateTransaction)
	r.PUT("/api/v1/transactions/:id", middleware.AdminAuthRequired(), UpdateTransaction)
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

// OPT-08: 软删除记录幂等重放时返回 410 Gone，并带 X-Idempotent-Replay 响应头
func TestOPT08_SoftDeletedRecordIdempotentReplayReturns410(t *testing.T) {
	r, token := setupContractRouter(t)

	body := map[string]any{
		"title": "即将被删的心愿", "client_request_id": "crid-opt08-deleted-wish",
	}
	w1 := doJSON(r, http.MethodPost, "/api/v1/wishes", token, body)
	if w1.Code != http.StatusOK {
		t.Fatalf("创建失败: %d", w1.Code)
	}
	if w1.Header().Get("X-Idempotent-Replay") != "" {
		t.Fatalf("首次创建不应带 X-Idempotent-Replay 头")
	}

	var created struct {
		Data models.Wish `json:"data"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &created)

	// 软删除该心愿
	if err := db.DB.Delete(&models.Wish{}, created.Data.ID).Error; err != nil {
		t.Fatalf("软删除失败: %v", err)
	}

	// 再次重放同一 client_request_id
	w2 := doJSON(r, http.MethodPost, "/api/v1/wishes", token, body)
	if w2.Code != http.StatusGone {
		t.Fatalf("软删记录幂等重放应返回 410 Gone, 实际得到: %d %s", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("软删重放应带 X-Idempotent-Replay: true")
	}
}

// ── 契约 B：同一 client_request_id 重复许愿 → 返回同一条记录，且不重复伴生散页
func TestCreateWishIdempotentByClientRequestID(t *testing.T) {
	r, token := setupContractRouter(t)

	body := map[string]any{
		"title": "幂等心愿", "client_request_id": "crid-wish-0001",
	}
	w1 := doJSON(r, http.MethodPost, "/api/v1/wishes", token, body)
	if w1.Code != http.StatusOK {
		t.Fatalf("首次许愿失败: %d %s", w1.Code, w1.Body.String())
	}
	var first struct {
		Data models.Wish `json:"data"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &first)

	w2 := doJSON(r, http.MethodPost, "/api/v1/wishes", token, body)
	if w2.Code != http.StatusOK {
		t.Fatalf("重复提交应幂等返回 200: %d %s", w2.Code, w2.Body.String())
	}
	var second struct {
		Data models.Wish `json:"data"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &second)

	if first.Data.ID != second.Data.ID {
		t.Fatalf("同一 client_request_id 应返回同一条心愿: 首次=%d, 重试=%d", first.Data.ID, second.Data.ID)
	}
	var count int64
	db.DB.Model(&models.Wish{}).Where("client_request_id = ?", "crid-wish-0001").Count(&count)
	if count != 1 {
		t.Fatalf("幂等许愿不得重复落库: 实际 %d 条", count)
	}
	var entryCount int64
	db.DB.Model(&models.LifeEntry{}).Where("wish_id = ?", first.Data.ID).Count(&entryCount)
	if entryCount != 1 {
		t.Fatalf("幂等许愿不得重复创建伴生散页: 实际 %d 条", entryCount)
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

// ── 发现 5：公开详情接口非数字 id → 400
func TestPublicDetailEndpointsRejectNonNumericID(t *testing.T) {
	r, token := setupContractRouter(t)
	_ = token
	enableAllPublicModulesForTest(t)

	// 注入形态（URL 编码后的 "2 OR 1=1"）：必须被入口拦截为 400
	req := httptest.NewRequest(http.MethodGet, "/api/v1/entries/2%20OR%201=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非数字 id 应 400, 实际 %d", w.Code)
	}
}

func TestUpdateWishTextFieldsPreserveNull(t *testing.T) {
	r, token := setupContractRouter(t)
	wish := models.Wish{
		Title: "原标题", Kind: "travel", Note: "原备注", Spec: "原规格",
		Budget: 12345, Status: "wanting",
	}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/wishes/" + u64(wish.ID)

	w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"title": nil, "note": nil, "spec": nil, "budget": nil,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("null 更新失败: %d %s", w.Code, w.Body.String())
	}
	var kept models.Wish
	db.DB.First(&kept, wish.ID)
	if kept.Title != wish.Title || kept.Note != wish.Note || kept.Spec != wish.Spec ||
		kept.Budget != wish.Budget {
		t.Fatalf("null 不得清空心愿字段: %+v", kept)
	}

	w = doJSON(r, http.MethodPut, path, token, map[string]any{
		"note": "", "spec": "", "budget": 0,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("清空失败: %d %s", w.Code, w.Body.String())
	}
	var cleared models.Wish
	db.DB.First(&cleared, wish.ID)
	if cleared.Note != "" || cleared.Spec != "" || cleared.Budget != 0 {
		t.Fatalf("显式空串应清空心愿字段: %+v", cleared)
	}
}

func TestUpdateEntryTextFieldsPreserveNull(t *testing.T) {
	r, token := setupContractRouter(t)
	entry := models.LifeEntry{Type: "life", Title: "原标题", Content: "原正文", Location: "原地点"}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/entries/" + u64(entry.ID)
	w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"title": nil, "content": nil, "location": nil,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	var kept models.LifeEntry
	if err := db.DB.First(&kept, entry.ID).Error; err != nil {
		t.Fatal(err)
	}
	if kept.Title != entry.Title || kept.Content != entry.Content || kept.Location != entry.Location {
		t.Fatalf("null 不得清空文本: title=%q content=%q location=%q", kept.Title, kept.Content, kept.Location)
	}
	w = doJSON(r, http.MethodPut, path, token, map[string]any{
		"title": "", "content": "", "location": "",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("清空失败: %d %s", w.Code, w.Body.String())
	}
	var cleared models.LifeEntry
	if err := db.DB.First(&cleared, entry.ID).Error; err != nil {
		t.Fatal(err)
	}
	if cleared.Title != "" || cleared.Content != "" || cleared.Location != "" {
		t.Fatalf("显式空串应清空文本: %+v", cleared)
	}
}

func TestUpdateTransactionTextFieldsPreserveNull(t *testing.T) {
	r, token := setupContractRouter(t)
	transaction := models.Transaction{Amount: 100, Type: "expense", Category: "餐饮", Title: "原标题", Note: "原备注", PaymentMethod: "现金"}
	if err := db.DB.Create(&transaction).Error; err != nil {
		t.Fatal(err)
	}
	w := doJSON(r, http.MethodPut, "/api/v1/transactions/"+u64(transaction.ID), token, map[string]any{
		"amount": 200, "category": "餐饮", "title": nil, "note": nil, "payment_method": nil,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	var kept models.Transaction
	if err := db.DB.First(&kept, transaction.ID).Error; err != nil {
		t.Fatal(err)
	}
	if kept.Amount != 200 || kept.Title != transaction.Title || kept.Note != transaction.Note || kept.PaymentMethod != transaction.PaymentMethod {
		t.Fatalf("金额应更新且 null 文本应保留: %+v", kept)
	}
}

func TestUpdateEntryAttachmentOwnership(t *testing.T) {
	for _, tc := range []struct {
		name       string
		owner      string
		related    bool
		clearFirst bool
		detachWish bool
		unbound    bool
	}{
		{name: "related_wish", owner: "wish", related: true},
		{name: "detach_wish_keep_image", owner: "wish", related: true, detachWish: true},
		{name: "detach_wish_rebind_image", owner: "wish", related: true, detachWish: true, clearFirst: true},
		{name: "detached_wish_rejects_new_image", owner: "wish", related: true, detachWish: true, unbound: true},
		{name: "foreign_wish", owner: "wish"},
		{name: "foreign_entry", owner: "entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, token := setupContractRouter(t)
			ownerID := uint(700)
			entry := models.LifeEntry{Type: "life", Title: "原记录", Content: "原内容"}
			if tc.related && tc.owner == "wish" {
				entry.WishID = &ownerID
			}
			if err := db.DB.Create(&entry).Error; err != nil {
				t.Fatal(err)
			}
			attachment := models.Attachment{ObjectKey: "entries/ownership.png", MimeType: "image/png"}
			if tc.related && !tc.unbound {
				attachment.EntryID = &entry.ID
			}
			switch tc.owner {
			case "wish":
				attachment.WishID = &ownerID
			case "entry":
				attachment.EntryID = &ownerID
			}
			if err := db.DB.Create(&attachment).Error; err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{
				"content": "修订正文", "attachment_ids": []uint{attachment.ID}, "clear_attachments": tc.clearFirst,
			}
			if tc.detachWish {
				payload["wish_id"] = nil
			}
			w := doJSON(r, http.MethodPut, "/api/v1/entries/"+u64(entry.ID), token, payload)
			wantStatus := http.StatusBadRequest
			if tc.related && !tc.unbound {
				wantStatus = http.StatusOK
			}
			if w.Code != wantStatus {
				t.Fatalf("归属校验状态应为 %d: %d %s", wantStatus, w.Code, w.Body.String())
			}
			var reloaded models.Attachment
			if err := db.DB.First(&reloaded, attachment.ID).Error; err != nil {
				t.Fatal(err)
			}
			sameID := func(a, b *uint) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
			if !sameID(reloaded.EntryID, attachment.EntryID) || !sameID(reloaded.WishID, attachment.WishID) {
				t.Fatalf("原有附件归属不得被改写: %+v", reloaded)
			}
			var updated models.LifeEntry
			if err := db.DB.First(&updated, entry.ID).Error; err != nil {
				t.Fatal(err)
			}
			if wantStatus == http.StatusBadRequest && updated.Content != entry.Content {
				t.Fatal("归属冲突应回滚正文修改")
			}
			if wantStatus == http.StatusOK && tc.detachWish && updated.WishID != nil {
				t.Fatal("引用 null 仍应解绑心愿")
			}
		})
	}
}
