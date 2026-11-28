package handlers

// 表单级自相矛盾载荷的服务端兜底校验回归测试（2026-10-25 专项审计全量修复 + 彻底根治）：
// 1. 分期仅支持支出（create/update，含「把分期账改成 income」一并 400）；
// 2. 分期渠道存在性：伪造 code 400（custom 已升级为真实渠道行，与普通渠道同规则）；
// 3. 还款日单源化：渠道存在时无视客户端矛盾值，无渠道时 1~31 钳制；
// 4. 分期参数：期数上限 60、installment_total_amount 必须与 amount 一致；
// 5. UpdateFootprint 返程早于到访自动交换（与 create 同口径）；
// 6. ResolveWish 重复归册 400、负数 actual_cost/budget/price 400、resolution_type 白名单 400；
// 7. 散页 type 与 project_id/wish_id 配对、外键存在性、wish_id=0 归一化解绑；
// 8. Project status 严格中文四态白名单（存量英文/历史值由启动迁移归一，测试见迁移用例）；
// 9. 足迹城市统一过国标字典（字典名优先）；
// 10. clear_city 只清城市归属不清 location（契约 A 修正）；
// 11. wish/footprint 互斥 400（只拦本次请求造成的双挂，存量历史行普通编辑放行）；
// 12. 账目 category 字典收口（expense∪gear / income，空 scope 回落内建默认列表）；
// 13. 启动迁移：custom 渠道行补建（还款日取历史众数）、项目状态归一。

import (
	"encoding/json"
	"net/http"
	"testing"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// seedCreditChannel 在测试库种一枚信贷渠道（存在性校验与还款日单源化的前提数据）
func seedCreditChannel(t *testing.T, code string, day int) {
	t.Helper()
	if err := db.DB.Create(&models.CreditChannel{
		Code: code, Name: "测试渠道 " + code, RepaymentDay: day, IsBuiltin: false,
	}).Error; err != nil {
		t.Fatalf("种子渠道 %s 失败: %v", code, err)
	}
}

// ── 1. 分期仅支持支出 ───────────────────────────────────────────────

func TestCreateTransactionInstallmentRequiresExpense(t *testing.T) {
	r, token := setupContractRouter(t)

	// income + is_installment=true → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "income", "category": "工资", "is_installment": true,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("income+分期创建应 400: %d %s", w.Code, w.Body.String())
	}
	// income + 携带分期字段（即便 is_installment 未置真）→ 400
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "income", "category": "工资", "installment_channel_code": "huabei",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("income 携带分期字段应 400: %d %s", w.Code, w.Body.String())
	}
	// income 纯净载荷 → 200
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "income", "category": "工资",
	}); w.Code != http.StatusOK {
		t.Fatalf("纯净 income 创建应 200: %d %s", w.Code, w.Body.String())
	}
}

func TestUpdateTransactionInstallmentTypeSwitchRejected(t *testing.T) {
	r, token := setupContractRouter(t)
	seedCreditChannel(t, "huabei", 10)

	tx := models.Transaction{
		Amount: 120000, Type: "expense", Category: "购物",
		IsInstallment: true, InstallmentChannelCode: "huabei",
		InstallmentTotalAmount: 120000, InstallmentMonths: 12,
		InstallmentMonthlyPay: 10000, InstallmentRemainMonths: 12, RepaymentDay: 10,
	}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/transactions/" + u64(tx.ID)

	// 把分期账改成 income → 400，账目保持 expense+分期
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 120000, "category": "购物", "type": "income",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("分期账改 income 应 400: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Transaction
	db.DB.First(&reloaded, tx.ID)
	if reloaded.Type != "expense" || !reloaded.IsInstallment {
		t.Fatalf("被拒绝的更新不得改写 type/is_installment: type=%s installment=%v", reloaded.Type, reloaded.IsInstallment)
	}

	// 显式 clear_installment + 改 income（分类同步换成 income 字典内的「退款」）：
	// 先取消分期再改类型是合法操作 → 200
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 120000, "category": "退款", "type": "income", "clear_installment": true,
	}); w.Code != http.StatusOK {
		t.Fatalf("clear_installment 后改 income 应 200: %d %s", w.Code, w.Body.String())
	}
	db.DB.First(&reloaded, tx.ID)
	if reloaded.Type != "income" || reloaded.IsInstallment {
		t.Fatalf("显式取消分期后应允许改 income: type=%s installment=%v", reloaded.Type, reloaded.IsInstallment)
	}
}

// ── 2/3. 分期渠道存在性 + 还款日单源化 ─────────────────────────────

func TestCreateTransactionRepaymentDayDerivesFromChannel(t *testing.T) {
	r, token := setupContractRouter(t)
	seedCreditChannel(t, "huabei", 10)

	create := func(body map[string]any) *models.Transaction {
		w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, body)
		if w.Code != http.StatusOK {
			t.Fatalf("创建失败: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data models.Transaction `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return &resp.Data
	}

	// 渠道存在：客户端自带矛盾还款日 25 被忽略，强制派生渠道还款日 10
	tx := create(map[string]any{
		"amount": 120000, "type": "expense", "category": "购物",
		"is_installment": true, "installment_channel_code": "huabei",
		"installment_months": 12, "installment_total_amount": 120000, "repayment_day": 25,
	})
	if tx.RepaymentDay != 10 {
		t.Fatalf("渠道存在时还款日应单源化为渠道值 10, 实际 %d", tx.RepaymentDay)
	}

	// custom 已升级为真实渠道行（启动迁移保证）：与普通渠道同规则 —— 单源化覆盖客户端值
	seedCreditChannel(t, "custom", 15)
	tx = create(map[string]any{
		"amount": 80000, "type": "expense", "category": "购物",
		"is_installment": true, "installment_channel_code": "custom",
		"installment_months": 6, "installment_total_amount": 80000, "repayment_day": 99,
	})
	if tx.RepaymentDay != 15 {
		t.Fatalf("custom 渠道存在时客户端越界还款日应被单源化为 15, 实际 %d", tx.RepaymentDay)
	}
	tx = create(map[string]any{
		"amount": 80000, "type": "expense", "category": "购物",
		"is_installment": true, "installment_channel_code": "custom",
		"installment_months": 6, "installment_total_amount": 80000,
	})
	if tx.RepaymentDay != 15 {
		t.Fatalf("custom 渠道缺省还款日应单源化为 15, 实际 %d", tx.RepaymentDay)
	}

	// 伪造渠道 → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "expense", "category": "购物",
		"is_installment": true, "installment_channel_code": "ghost_bank",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("伪造分期渠道应 400: %d %s", w.Code, w.Body.String())
	}
}

func TestUpdateTransactionRepaymentDayDerivesFromChannel(t *testing.T) {
	r, token := setupContractRouter(t)
	seedCreditChannel(t, "huabei", 10)

	tx := models.Transaction{
		Amount: 120000, Type: "expense", Category: "购物",
		IsInstallment: true, InstallmentChannelCode: "huabei",
		InstallmentTotalAmount: 120000, InstallmentMonths: 12,
		InstallmentMonthlyPay: 10000, InstallmentRemainMonths: 12, RepaymentDay: 10,
	}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/transactions/" + u64(tx.ID)

	// 客户端自带矛盾还款日 25：渠道存在 → 强制回渠道值 10
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 120000, "category": "购物", "type": "expense",
		"is_installment": true, "repayment_day": 25,
	}); w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Transaction
	db.DB.First(&reloaded, tx.ID)
	if reloaded.RepaymentDay != 10 {
		t.Fatalf("更新时渠道存在应单源化还款日为 10, 实际 %d", reloaded.RepaymentDay)
	}

	// 期数越上限 → 400
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 120000, "category": "购物", "type": "expense",
		"is_installment": true, "installment_months": 99,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("分期期数 >60 应 400: %d %s", w.Code, w.Body.String())
	}

	// 分期总额与账单金额不一致 → 400
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"amount": 120000, "category": "购物", "type": "expense",
		"is_installment": true, "installment_total_amount": 999,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("分期总额与金额不一致应 400: %d %s", w.Code, w.Body.String())
	}
	db.DB.First(&reloaded, tx.ID)
	if reloaded.InstallmentMonths != 12 || reloaded.InstallmentTotalAmount != 120000 {
		t.Fatalf("被拒绝的更新不得改写分期字段: months=%d total=%d", reloaded.InstallmentMonths, reloaded.InstallmentTotalAmount)
	}
}

// ResolveChannelRepaymentDay / InstallmentChannelExists 纯单元校验
func TestResolveChannelRepaymentDayAndExistence(t *testing.T) {
	// db.DB 指向的是上一个用例已清理的临时库，必须先挂上本用例自己的临时库
	setupContractRouter(t)

	// custom 已无特例：无行时与普通渠道同规则 —— 不存在、钳制兜底
	if InstallmentChannelExists(nil, "custom") {
		t.Fatal("无 custom 渠道行时应判定为不存在（特例已移除）")
	}
	clampCases := []struct {
		in   int
		want int
	}{
		{99, 31}, {0, 10}, {-5, 10}, {15, 15},
	}
	for _, tc := range clampCases {
		if got := ResolveChannelRepaymentDay(nil, "custom", tc.in); got != tc.want {
			t.Fatalf("无行时 ResolveChannelRepaymentDay(custom, %d) = %d, want %d", tc.in, got, tc.want)
		}
	}

	// custom 行存在（启动迁移保证）后：单源化覆盖客户端值
	seedCreditChannel(t, "custom", 15)
	if !InstallmentChannelExists(nil, "custom") {
		t.Fatal("custom 渠道行存在时应判定为存在")
	}
	if got := ResolveChannelRepaymentDay(nil, "custom", 99); got != 15 {
		t.Fatalf("custom 渠道存在时应单源化为 15, 实际 %d", got)
	}

	// 普通渠道：存在 → 单源化；未知 → 拒绝
	seedCreditChannel(t, "cmb_unit", 18)
	if got := ResolveChannelRepaymentDay(nil, "cmb_unit", 25); got != 18 {
		t.Fatalf("渠道存在时应返回渠道还款日 18, 实际 %d", got)
	}
	if !InstallmentChannelExists(nil, "") {
		t.Fatal("空 code 应视为合法（组装时回落内置 huabei）")
	}
	if InstallmentChannelExists(nil, "ghost_bank") {
		t.Fatal("不存在的渠道应判定为不存在")
	}
}

// ── 4. 心愿归册：重复归册 + 负数金额 + 分期参数 ─────────────────────

func TestResolveWishTwiceRejected(t *testing.T) {
	r, token := setupContractRouter(t)

	wish := models.Wish{Title: "只归册一次", Kind: "item", Budget: 100000, Status: "wanting"}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"resolution_type": "exact", "actual_cost": 88000, "create_transaction": true,
	}
	if w := doJSON(r, http.MethodPost, "/api/v1/wishes/"+u64(wish.ID)+"/resolve", token, body); w.Code != http.StatusOK {
		t.Fatalf("首次归册应 200: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(r, http.MethodPost, "/api/v1/wishes/"+u64(wish.ID)+"/resolve", token, body); w.Code != http.StatusBadRequest {
		t.Fatalf("重复归册应 400: %d %s", w.Code, w.Body.String())
	}
	var count int64
	db.DB.Model(&models.Transaction{}).Where("wish_id = ?", wish.ID).Count(&count)
	if count != 1 {
		t.Fatalf("重复归册不得产生第二笔流水, 实际 %d 笔", count)
	}
}

func TestResolveWishRejectsNegativeCostAndBadInstallment(t *testing.T) {
	r, token := setupContractRouter(t)

	wish := models.Wish{Title: "负数校验", Kind: "item", Status: "wanting"}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/wishes/" + u64(wish.ID) + "/resolve"

	if w := doJSON(r, http.MethodPost, path, token, map[string]any{
		"resolution_type": "exact", "actual_cost": -100,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("负数 actual_cost 应 400: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(r, http.MethodPost, path, token, map[string]any{
		"resolution_type": "exact", "actual_cost": 100, "create_transaction": true,
		"is_installment": true, "installment_channel_code": "ghost_bank",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("归册分期伪造渠道应 400: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(r, http.MethodPost, path, token, map[string]any{
		"resolution_type": "exact", "actual_cost": 100, "create_transaction": true,
		"is_installment": true, "installment_months": 99,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("归册分期期数 >60 应 400: %d %s", w.Code, w.Body.String())
	}

	var reloaded models.Wish
	db.DB.First(&reloaded, wish.ID)
	if reloaded.Status != "wanting" {
		t.Fatalf("被拒绝的归册不得改写心愿状态: %s", reloaded.Status)
	}
}

func TestWishRejectsNegativeBudgetAndCost(t *testing.T) {
	r, token := setupContractRouter(t)

	// 创建：负预算 400
	if w := doJSON(r, http.MethodPost, "/api/v1/wishes", token, map[string]any{
		"title": "负预算心愿", "kind": "item", "budget": -1,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("负预算创建应 400: %d %s", w.Code, w.Body.String())
	}

	wish := models.Wish{Title: "预算改零", Kind: "item", Budget: 100, Status: "wanting"}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/wishes/" + u64(wish.ID)

	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"budget": -5}); w.Code != http.StatusBadRequest {
		t.Fatalf("负预算更新应 400: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"actual_cost": -5}); w.Code != http.StatusBadRequest {
		t.Fatalf("负 actual_cost 更新应 400: %d %s", w.Code, w.Body.String())
	}
	// 预算改 0 合法（契约：0 = 清空预算）
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{"budget": 0}); w.Code != http.StatusOK {
		t.Fatalf("预算改 0 应 200: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Wish
	db.DB.First(&reloaded, wish.ID)
	if reloaded.Budget != 0 {
		t.Fatalf("预算应已改 0, 实际 %d", reloaded.Budget)
	}
}

// ── 6. 散页 type 与归属外键配对 + 外键存在性 ────────────────────────

func TestEntryTypeBindingPairing(t *testing.T) {
	r, token := setupContractRouter(t)

	wish := models.Wish{Title: "配对心愿", Kind: "item", Status: "wanting"}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}

	// type=life 带 wish_id → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/entries", token, map[string]any{
		"type": "life", "content": "x", "wish_id": wish.ID,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("life+wish_id 应 400: %d %s", w.Code, w.Body.String())
	}
	// 配对正确 → 200
	if w := doJSON(r, http.MethodPost, "/api/v1/entries", token, map[string]any{
		"type": "wish", "content": "x", "wish_id": wish.ID,
	}); w.Code != http.StatusOK {
		t.Fatalf("wish+wish_id 应 200: %d %s", w.Code, w.Body.String())
	}

	// 切走类型但未显式解绑 → 400
	entry := models.LifeEntry{Type: "wish", Title: "待切类型", Content: "x", WishID: &wish.ID}
	if err := db.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/entries/" + u64(entry.ID)
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"type": "life", "content": "改成日常",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("切类型未解绑 wish_id 应 400: %d %s", w.Code, w.Body.String())
	}
	// 显式携带 wish_id=null 解绑后切类型 → 200 且归属已清
	if w := doJSON(r, http.MethodPut, path, token, map[string]any{
		"type": "life", "content": "改成日常", "wish_id": nil,
	}); w.Code != http.StatusOK {
		t.Fatalf("显式解绑后切类型应 200: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.LifeEntry
	db.DB.First(&reloaded, entry.ID)
	if reloaded.Type != "life" || reloaded.WishID != nil {
		t.Fatalf("解绑后应为 life 且 wish_id 为空: type=%s wish=%v", reloaded.Type, reloaded.WishID)
	}

	// life 条目携带 wish_id → 400
	lifeEntry := models.LifeEntry{Type: "life", Title: "纯日常", Content: "x"}
	if err := db.DB.Create(&lifeEntry).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodPut, "/api/v1/entries/"+u64(lifeEntry.ID), token, map[string]any{
		"content": "x", "wish_id": wish.ID,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("life 条目携带 wish_id 应 400: %d %s", w.Code, w.Body.String())
	}
}

func TestEntryAndTransactionForeignKeyExistence(t *testing.T) {
	r, token := setupContractRouter(t)

	// 散页外键不存在 → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/entries", token, map[string]any{
		"type": "wish", "content": "x", "wish_id": 999999,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("散页 wish_id=999999 应 400: %d %s", w.Code, w.Body.String())
	}
	// 账目外键不存在 → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "expense", "category": "餐饮", "wish_id": 999999,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("账目 wish_id=999999 应 400: %d %s", w.Code, w.Body.String())
	}

	// wish_id=0 归一化为 nil（创建）
	w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "expense", "category": "餐饮", "wish_id": 0,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("wish_id=0 创建应 200: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data models.Transaction `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.WishID != nil {
		t.Fatalf("wish_id=0 应归一化为 nil, 实际 %v", *created.Data.WishID)
	}

	// wish_id=0 归一化为解绑（更新）
	wish := models.Wish{Title: "待解绑心愿", Kind: "item", Status: "wanting"}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	tx := models.Transaction{Amount: 100, Type: "expense", Category: "餐饮", WishID: &wish.ID}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodPut, "/api/v1/transactions/"+u64(tx.ID), token, map[string]any{
		"amount": 100, "category": "餐饮", "wish_id": 0,
	}); w.Code != http.StatusOK {
		t.Fatalf("wish_id=0 解绑应 200: %d %s", w.Code, w.Body.String())
	}
	var reloaded models.Transaction
	db.DB.First(&reloaded, tx.ID)
	if reloaded.WishID != nil {
		t.Fatalf("wish_id=0 更新后应解绑, 实际 %v", *reloaded.WishID)
	}
}

// ── 13. 启动迁移：custom 渠道行补建 ─────────────────────────────────

func TestEnsureCustomInstallmentChannelMigration(t *testing.T) {
	t.Run("Legacy custom txs derive mode repayment day", func(t *testing.T) {
		setupContractRouter(t)
		for _, day := range []int{25, 25, 10} {
			tx := models.Transaction{
				Amount: 100000, Type: "expense", Category: "购物",
				IsInstallment: true, InstallmentChannelCode: "custom",
				InstallmentMonths: 12, InstallmentTotalAmount: 100000, RepaymentDay: day,
			}
			if err := db.DB.Create(&tx).Error; err != nil {
				t.Fatal(err)
			}
		}
		db.EnsureCustomInstallmentChannel(db.DB)
		var ch models.CreditChannel
		if err := db.DB.Where("code = ?", "custom").First(&ch).Error; err != nil {
			t.Fatalf("迁移应补建 custom 渠道行: %v", err)
		}
		if ch.RepaymentDay != 25 {
			t.Fatalf("还款日应取历史 custom 账目众数 25, 实际 %d", ch.RepaymentDay)
		}
		if !ch.IsBuiltin {
			t.Fatal("custom 渠道行应为内置（不可删除，否则 Web 自定义分期表单会 400）")
		}
		if ch.Name != "自定义分期" {
			t.Fatalf("custom 渠道名应为「自定义分期」, 实际 %q", ch.Name)
		}

		// 幂等：重复执行不重复建行、不漂移
		db.EnsureCustomInstallmentChannel(db.DB)
		var count int64
		db.DB.Model(&models.CreditChannel{}).Where("code = ?", "custom").Count(&count)
		if count != 1 {
			t.Fatalf("迁移必须幂等, custom 渠道行实际 %d 条", count)
		}
	})

	t.Run("Fresh database falls back to day 10", func(t *testing.T) {
		setupContractRouter(t)
		db.EnsureCustomInstallmentChannel(db.DB)
		var ch models.CreditChannel
		if err := db.DB.Where("code = ?", "custom").First(&ch).Error; err != nil {
			t.Fatalf("全新库也应补建 custom 渠道行: %v", err)
		}
		if ch.RepaymentDay != 10 {
			t.Fatalf("无历史账目时还款日应兜底 10, 实际 %d", ch.RepaymentDay)
		}
	})
}

// ── 4(补). resolution_type 白名单 ───────────────────────────────────

func TestResolveWishRejectsInvalidResolutionType(t *testing.T) {
	r, token := setupContractRouter(t)

	wish := models.Wish{Title: "归册方式校验", Kind: "item", Status: "wanting"}
	if err := db.DB.Create(&wish).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/wishes/" + u64(wish.ID) + "/resolve"

	// 非法值 400（不再静默钳到 exact）—— 拼错「relinquished」绝不能生成支出流水
	if w := doJSON(r, http.MethodPost, path, token, map[string]any{
		"resolution_type": "relinquisehd", "actual_cost": 100, "create_transaction": true,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 resolution_type 应 400: %d %s", w.Code, w.Body.String())
	}
	var count int64
	db.DB.Model(&models.Transaction{}).Where("wish_id = ?", wish.ID).Count(&count)
	if count != 0 {
		t.Fatalf("被拒绝的归册不得生成流水, 实际 %d 笔", count)
	}
	var reloaded models.Wish
	db.DB.First(&reloaded, wish.ID)
	if reloaded.Status != "wanting" {
		t.Fatalf("被拒绝的归册不得改写心愿状态: %s", reloaded.Status)
	}

	// 缺省仍回落 exact
	if w := doJSON(r, http.MethodPost, path, token, map[string]any{
		"actual_cost": 100, "create_transaction": true,
	}); w.Code != http.StatusOK {
		t.Fatalf("缺省 resolution_type 应 200: %d %s", w.Code, w.Body.String())
	}
}

// ── 12. 账目 category 字典收口 ──────────────────────────────────────

func TestTransactionCategoryDictionary(t *testing.T) {
	r, token := setupContractRouter(t)

	// 测试库未种子分类字典 → 与 GetCategories 同口径回落内建默认列表：
	// expense 默认里的「餐饮」放行
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "expense", "category": "餐饮",
	}); w.Code != http.StatusOK {
		t.Fatalf("内建默认分类应放行: %d %s", w.Code, w.Body.String())
	}
	// 不在字典 → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "expense", "category": "非字典分类",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("字典外分类应 400: %d %s", w.Code, w.Body.String())
	}
	// income 只认 income scope：expense 分类「餐饮」对 income 是错挂 → 400
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "income", "category": "餐饮",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("income 挂 expense 分类应 400: %d %s", w.Code, w.Body.String())
	}
	// income 内建分类放行
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "income", "category": "副业",
	}); w.Code != http.StatusOK {
		t.Fatalf("income 内建分类应放行: %d %s", w.Code, w.Body.String())
	}

	// 字典已种子化后以库内行为准：expense scope 只剩「测试支出类」→ 内建「餐饮」被拒
	if err := db.DB.Create(&models.CategoryOption{Scope: "expense", Name: "测试支出类", SortOrder: 10}).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "expense", "category": "餐饮",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("字典种子化后库外默认分类应 400: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(r, http.MethodPost, "/api/v1/transactions", token, map[string]any{
		"amount": 100, "type": "expense", "category": "测试支出类",
	}); w.Code != http.StatusOK {
		t.Fatalf("库内分类应放行: %d %s", w.Code, w.Body.String())
	}

	// 更新同口径：合法创建的账目改成字典外分类 → 400
	tx := models.Transaction{Amount: 100, Type: "expense", Category: "测试支出类"}
	if err := db.DB.Create(&tx).Error; err != nil {
		t.Fatal(err)
	}
	if w := doJSON(r, http.MethodPut, "/api/v1/transactions/"+u64(tx.ID), token, map[string]any{
		"amount": 100, "category": "非字典分类",
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("更新为字典外分类应 400: %d %s", w.Code, w.Body.String())
	}
}
