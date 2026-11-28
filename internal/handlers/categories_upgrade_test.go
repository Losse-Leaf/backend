package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

func setupCategoriesUpgradeRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	if err := db.DB.AutoMigrate(&models.CategoryRule{}); err != nil {
		t.Fatalf("迁移 CategoryRule 失败: %v", err)
	}
	db.DB.Create(&models.SiteConfig{SiteName: "测试站", Initialized: true})

	token := "lldt_categories_upgrade"
	db.DB.Create(&models.Device{
		DeviceID: "dev_cat_test", Name: "分类测试设备", Platform: "web",
		TokenHash: middleware.HashToken(token), Status: "active",
	})

	r := gin.New()
	r.GET("/api/v1/categories", GetCategories)
	write := r.Group("/api/v1", middleware.AdminAuthRequired())
	write.POST("/categories", CreateCategory)
	write.PUT("/categories/:id", UpdateCategory)
	write.DELETE("/categories/:id", DeleteCategory)
	write.GET("/category-rules", GetCategoryRules)
	write.PUT("/category-rules", UpsertCategoryRule)
	write.DELETE("/category-rules/:id", DeleteCategoryRule)
	write.POST("/transactions", CreateTransaction)
	write.PUT("/transactions/:id", UpdateTransaction)
	return r, token
}

func TestUpgradeCategoryCatalogV3IdempotentAndHiddenSystem(t *testing.T) {
	r, token := setupCategoriesUpgradeRouter(t)

	// 模拟 v1/v2 混合存量库（含旧名称、系统隐藏分类与 1 个用户自定义分类）
	legacyExpenses := []string{
		"餐饮", "咖啡饮品", "交通", "旅行度假", "购物", "数码器材",
		"住房", "订阅会员", "娱乐", "医疗", "学习", "人情社交", "好物购置", "利息手续费", "其他",
	}
	for idx, name := range legacyExpenses {
		db.DB.Create(&models.CategoryOption{Scope: "expense", Name: name, GroupName: "旧组", SortOrder: (idx + 1) * 10, IsBuiltin: true})
	}
	db.DB.Model(&models.CategoryOption{}).Where("scope = ? AND name = ?", "expense", "咖啡饮品").Update("keywords", "瑞幸,手冲")
	db.DB.Create(&models.CategoryOption{Scope: "expense", Name: "手办模型", SortOrder: 300, IsBuiltin: false})

	legacyIncomes := []string{"工资收入", "奖金补贴", "稿酬设计", "投资理财", "二手回血", "退款报销", "其他收入"}
	for idx, name := range legacyIncomes {
		db.DB.Create(&models.CategoryOption{Scope: "income", Name: name, GroupName: "旧组", SortOrder: (idx + 1) * 10, IsBuiltin: true})
	}
	legacyPayments := []string{"微信支付", "支付宝", "银行卡", "现金", "其他"}
	for idx, name := range legacyPayments {
		db.DB.Create(&models.CategoryOption{Scope: "payment", Name: name, SortOrder: (idx + 1) * 10, IsBuiltin: true})
	}

	// 历史流水与规则
	db.DB.Create(&models.Transaction{Amount: 3200, Type: "expense", Category: "咖啡饮品", Title: "瑞幸"})
	db.DB.Create(&models.Transaction{Amount: 500000, Type: "income", Category: "工资收入", Title: "月薪"})
	db.DB.Create(&models.CategoryRule{TxType: "expense", MatchKey: "亚朵酒店", DisplayName: "亚朵酒店", Category: "旅行度假"})
	db.DB.Create(&models.CategoryRule{TxType: "income", MatchKey: "约稿", DisplayName: "约稿", Category: "稿酬设计"})

	db.UpgradeCategoryCatalogV3(db.DB)

	var cfg models.SiteConfig
	db.DB.First(&cfg)
	if cfg.CategoryCatalogVersion != 3 {
		t.Fatalf("升级后 CategoryCatalogVersion 应为 3, got %d", cfg.CategoryCatalogVersion)
	}

	// 校验流水与规则已平滑迁移至 2 字大类
	var migratedExpense models.Transaction
	db.DB.Where("title = ?", "瑞幸").First(&migratedExpense)
	if migratedExpense.Category != "餐饮" {
		t.Fatalf("「咖啡饮品」流水应迁移至「餐饮」, got %q", migratedExpense.Category)
	}
	var migratedIncome models.Transaction
	db.DB.Where("title = ?", "月薪").First(&migratedIncome)
	if migratedIncome.Category != "工资" {
		t.Fatalf("「工资收入」流水应迁移至「工资」, got %q", migratedIncome.Category)
	}
	var hotelRule models.CategoryRule
	db.DB.Where("match_key = ?", "亚朵酒店").First(&hotelRule)
	if hotelRule.Category != "娱乐" {
		t.Fatalf("「旅行度假」规则应迁移至「娱乐」, got %q", hotelRule.Category)
	}
	var draftRule models.CategoryRule
	db.DB.Where("match_key = ?", "约稿").First(&draftRule)
	if draftRule.Category != "副业" {
		t.Fatalf("「稿酬设计」规则应迁移至「副业」, got %q", draftRule.Category)
	}

	// 校验旧分类 keywords 合并到目标分类
	var dining models.CategoryOption
	db.DB.Where("scope = ? AND name = ?", "expense", "餐饮").First(&dining)
	if dining.Keywords != "瑞幸,手冲" || dining.GroupName != "" {
		t.Fatalf("「餐饮」应合并旧关键词并清空 group_name, got %+v", dining)
	}

	// GET /api/v1/categories 必须完全隐藏「好物购置」「利息手续费」，且保留用户自定义「手办模型」
	wList := doJSON(r, http.MethodGet, "/api/v1/categories", token, nil)
	if wList.Code != http.StatusOK {
		t.Fatalf("GET /categories 失败: %d", wList.Code)
	}
	var listResp struct {
		Data struct {
			Items   []models.CategoryOption `json:"items"`
			ByScope map[string][]string     `json:"by_scope"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	for _, name := range listResp.Data.ByScope["expense"] {
		if name == "好物购置" || name == "利息手续费" {
			t.Fatalf("用户侧 GET /categories 不得暴露系统内部分类 %q", name)
		}
	}
	if len(listResp.Data.ByScope["income"]) != 5 {
		t.Fatalf("v3 收入大类应为 5 项, got %v", listResp.Data.ByScope["income"])
	}
	var customCount int64
	db.DB.Model(&models.CategoryOption{}).Where("scope = ? AND name = ?", "expense", "手办模型").Count(&customCount)
	if customCount != 1 {
		t.Fatalf("用户自定义分类「手办模型」必须保留, got %d", customCount)
	}

	// 用户手动删除「订阅」后再次执行升级，绝不能复活被删项
	var sub models.CategoryOption
	if err := db.DB.Where("scope = ? AND name = ?", "expense", "订阅").First(&sub).Error; err != nil {
		t.Fatalf("应存在「订阅」: %v", err)
	}
	wDel := doJSON(r, http.MethodDelete, fmt.Sprintf("/api/v1/categories/%d", sub.ID), token, nil)
	if wDel.Code != http.StatusOK {
		t.Fatalf("删除「订阅」应成功: %d %s", wDel.Code, wDel.Body.String())
	}

	db.UpgradeCategoryCatalogV3(db.DB)
	var subCount int64
	db.DB.Model(&models.CategoryOption{}).Where("scope = ? AND name = ?", "expense", "订阅").Count(&subCount)
	if subCount != 0 {
		t.Fatalf("二次执行 UpgradeCategoryCatalogV3 不得复活用户已删除分类, got %d", subCount)
	}
}

func TestCategoryCRUDMigrationAndUsageCount(t *testing.T) {
	r, token := setupCategoriesUpgradeRouter(t)
	db.SeedDefaultCategories(db.DB)
	db.DB.Create(&models.CategoryOption{Scope: "expense", Name: "好物购置", SortOrder: 900, IsBuiltin: true})

	// 创建带 keywords 的自定义分类
	wCreate := doJSON(r, http.MethodPost, "/api/v1/categories", token, map[string]any{
		"scope":    "expense",
		"name":     "手工模型",
		"keywords": "高达，万代；拼装\n模型",
	})
	if wCreate.Code != http.StatusOK {
		t.Fatalf("创建自定义分类失败: %d %s", wCreate.Code, wCreate.Body.String())
	}
	var createResp struct {
		Data models.CategoryOption `json:"data"`
	}
	if err := json.Unmarshal(wCreate.Body.Bytes(), &createResp); err != nil {
		t.Fatal(err)
	}
	if createResp.Data.Keywords != "高达,万代,拼装,模型" {
		t.Fatalf("创建返回字段异常: %+v", createResp.Data)
	}

	// 写入两条历史账目和一条分期计划 + 一条归类规则引用「手工模型」
	db.DB.Create(&models.Transaction{Amount: 29900, Type: "expense", Category: "手工模型", Title: "万代门店"})
	db.DB.Create(&models.Transaction{Amount: 19900, Type: "expense", Category: "手工模型", Title: "拼装工具"})
	db.DB.Create(&models.InstallmentPlan{
		ChannelID: 1, ChannelCode: "huabei", Title: "大件模型", Kind: "expense",
		Principal: 100000, Periods: 3, FeeMode: "none", FirstDueDate: "2026-12-10",
		RepayDay: 10, CategoryName: "手工模型", Status: "active",
	})
	db.DB.Create(&models.CategoryRule{
		TxType: "expense", MatchKey: "万代旗舰店", DisplayName: "万代旗舰店",
		Category: "手工模型", Source: "learned",
	})

	// GET /categories 应包含 usage_count = 3
	wList := doJSON(r, http.MethodGet, "/api/v1/categories?scope=expense", token, nil)
	if wList.Code != http.StatusOK {
		t.Fatalf("查询分类失败: %d", wList.Code)
	}
	var listResp struct {
		Data struct {
			Items []models.CategoryOption `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wList.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	var foundUsage int64 = -1
	for _, it := range listResp.Data.Items {
		if it.ID == createResp.Data.ID {
			foundUsage = it.UsageCount
		}
	}
	if foundUsage != 3 {
		t.Fatalf("「手工模型」usage_count 应为 3 (2笔流水+1个分期), got %d", foundUsage)
	}

	// 更新改名并同步归类规则
	wUpd := doJSON(r, http.MethodPut, fmt.Sprintf("/api/v1/categories/%d", createResp.Data.ID), token, map[string]any{
		"name":     "潮玩手办",
		"keywords": "高达,手办",
	})
	if wUpd.Code != http.StatusOK {
		t.Fatalf("更新分类失败: %d %s", wUpd.Code, wUpd.Body.String())
	}
	var rule models.CategoryRule
	db.DB.Where("match_key = ?", "万代旗舰店").First(&rule)
	if rule.Category != "潮玩手办" {
		t.Fatalf("改名后 category_rules 应同步更新为「潮玩手办」, got %q", rule.Category)
	}

	// 不带 migrate_to 删除仍被引用的分类应 400
	wFailDel := doJSON(r, http.MethodDelete, fmt.Sprintf("/api/v1/categories/%d", createResp.Data.ID), token, nil)
	if wFailDel.Code != http.StatusBadRequest {
		t.Fatalf("未指定 migrate_to 删除有引用分类应 400, got %d", wFailDel.Code)
	}

	// 带 ?migrate_to=娱乐 删除应迁移全部历史流水、分期与规则并成功删除
	wMigDel := doJSON(r, http.MethodDelete, fmt.Sprintf("/api/v1/categories/%d?migrate_to=娱乐", createResp.Data.ID), token, nil)
	if wMigDel.Code != http.StatusOK {
		t.Fatalf("带 migrate_to 删除失败: %d %s", wMigDel.Code, wMigDel.Body.String())
	}
	var migratedTxCount int64
	db.DB.Model(&models.Transaction{}).Where("category = ?", "娱乐").Count(&migratedTxCount)
	if migratedTxCount != 2 {
		t.Fatalf("迁移后「娱乐」流水应为 2 条, got %d", migratedTxCount)
	}
	var plan models.InstallmentPlan
	db.DB.First(&plan)
	if plan.CategoryName != "娱乐" {
		t.Fatalf("迁移后分期计划分类应为「娱乐」, got %q", plan.CategoryName)
	}
	db.DB.Where("match_key = ?", "万代旗舰店").First(&rule)
	if rule.Category != "娱乐" {
		t.Fatalf("迁移后规则分类应为「娱乐」, got %q", rule.Category)
	}

	// 系统内置分类「好物购置」即使带 migrate_to 也不可删
	var goodBuy models.CategoryOption
	db.DB.Where("scope = ? AND name = ?", "expense", "好物购置").First(&goodBuy)
	wBuiltinDel := doJSON(r, http.MethodDelete, fmt.Sprintf("/api/v1/categories/%d?migrate_to=其他", goodBuy.ID), token, nil)
	if wBuiltinDel.Code != http.StatusBadRequest {
		t.Fatalf("系统内置分类带 migrate_to 仍应拒绝删除, got %d", wBuiltinDel.Code)
	}
}

func TestUpdateTransactionLearnsMerchantRuleAndRulesCRUD(t *testing.T) {
	r, token := setupCategoriesUpgradeRouter(t)
	db.SeedDefaultCategories(db.DB)

	tx := models.Transaction{
		Amount: 1800, Type: "expense", Category: "其他",
		Title: "楼下张记面馆（万达店）", PaymentMethod: "微信支付",
	}
	db.DB.Create(&tx)

	wPut := doJSON(r, http.MethodPut, fmt.Sprintf("/api/v1/transactions/%d", tx.ID), token, map[string]any{
		"amount":   1800,
		"type":     "expense",
		"category": "餐饮",
	})
	if wPut.Code != http.StatusOK {
		t.Fatalf("更新账目失败: %d %s", wPut.Code, wPut.Body.String())
	}

	var learned models.CategoryRule
	if err := db.DB.Where("tx_type = ? AND match_key = ?", "expense", "楼下张记面馆").First(&learned).Error; err != nil {
		t.Fatalf("修改分类后应自动学习商户规则: %v", err)
	}
	if learned.Category != "餐饮" || learned.Source != "learned" {
		t.Fatalf("学习结果异常: %+v", learned)
	}

	genericTx := models.Transaction{
		Amount: 2500, Type: "expense", Category: "其他",
		Title: "微信支付自动记账", PaymentMethod: "微信支付",
	}
	db.DB.Create(&genericTx)
	doJSON(r, http.MethodPut, fmt.Sprintf("/api/v1/transactions/%d", genericTx.ID), token, map[string]any{
		"amount":   2500,
		"type":     "expense",
		"category": "餐饮",
	})
	var genericCount int64
	db.DB.Model(&models.CategoryRule{}).Where("match_key = ?", "微信支付自动记账").Count(&genericCount)
	if genericCount != 0 {
		t.Fatalf("通用占位标题不得学入 category_rules, got %d", genericCount)
	}

	wRuleUpsert := doJSON(r, http.MethodPut, "/api/v1/category-rules", token, map[string]any{
		"tx_type":      "expense",
		"display_name": "山姆会员商店",
		"category":     "购物",
	})
	if wRuleUpsert.Code != http.StatusOK {
		t.Fatalf("手动创建归类规则失败: %d %s", wRuleUpsert.Code, wRuleUpsert.Body.String())
	}

	wRuleList := doJSON(r, http.MethodGet, "/api/v1/category-rules", token, nil)
	if wRuleList.Code != http.StatusOK {
		t.Fatalf("读取归类规则失败: %d", wRuleList.Code)
	}
	var rulesResp struct {
		Data []models.CategoryRule `json:"data"`
	}
	if err := json.Unmarshal(wRuleList.Body.Bytes(), &rulesResp); err != nil {
		t.Fatal(err)
	}
	if len(rulesResp.Data) != 2 {
		t.Fatalf("应有 2 条归类规则, got %d", len(rulesResp.Data))
	}

	wRuleDel := doJSON(r, http.MethodDelete, fmt.Sprintf("/api/v1/category-rules/%d", rulesResp.Data[0].ID), token, nil)
	if wRuleDel.Code != http.StatusOK {
		t.Fatalf("删除归类规则失败: %d %s", wRuleDel.Code, wRuleDel.Body.String())
	}
}
