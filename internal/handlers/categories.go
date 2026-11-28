package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

var validCategoryScopes = map[string]bool{
	"expense": true,
	"income":  true,
	"payment": true,
}

// normalizeCategoryKeywords 统一规范化分类自动匹配关键词（逗号分隔、去重、去空白、限长）
func normalizeCategoryKeywords(raw string) string {
	replaced := strings.NewReplacer(
		"，", ",",
		"、", ",",
		"；", ",",
		";", ",",
		"\r\n", ",",
		"\n", ",",
		"\r", ",",
		"\t", ",",
	).Replace(raw)
	parts := strings.Split(replaced, ",")
	seen := make(map[string]bool, len(parts))
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		kw := strings.TrimSpace(p)
		if kw == "" {
			continue
		}
		lower := strings.ToLower(kw)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		cleaned = append(cleaned, kw)
	}
	joined := strings.Join(cleaned, ",")
	runes := []rune(joined)
	if len(runes) > 500 {
		return string(runes[:500])
	}
	return joined
}

// transactionCategoryAllowed 账目分类字典收口（表单契约修复）：
// category 必须真实存在于有效字典，杜绝「income+餐饮」这类自由文本错挂。
func transactionCategoryAllowed(txType, category string) bool {
	name := strings.TrimSpace(category)
	if name == "" {
		return false
	}
	scope := "expense"
	if txType == "income" {
		scope = "income"
	}
	// 系统自动账单专用分类（如「好物购置」「利息手续费」）不在用户列表暴露，但内部校验必须始终放行
	if scope == "expense" && db.HiddenSystemExpenseCategories[name] {
		return true
	}
	var rowCount int64
	rowQuery := db.DB.Model(&models.CategoryOption{}).Where("scope = ?", scope)
	if scope == "expense" {
		rowQuery = rowQuery.Where("name NOT IN ?", db.HiddenSystemExpenseNames())
	}
	rowQuery.Count(&rowCount)
	if rowCount == 0 {
		for _, def := range db.DefaultCategoriesMap[scope] {
			if def == name {
				return true
			}
		}
		return false
	}
	var hit int64
	db.DB.Model(&models.CategoryOption{}).Where("scope = ? AND name = ?", scope, name).Count(&hit)
	return hit > 0
}

// GetCategories 获取分类字典列表 (支持 ?scope= 过滤，同时返回列表和按 scope 分组的字典)
func GetCategories(c *gin.Context) {
	scope := strings.TrimSpace(c.Query("scope"))
	query := db.DB.Model(&models.CategoryOption{}).
		Where("scope IN ?", []string{"expense", "income", "payment"}).
		Where("NOT (scope = 'expense' AND name IN ?)", db.HiddenSystemExpenseNames()).
		Order("scope ASC, sort_order ASC, id ASC")
	if scope != "" {
		query = query.Where("scope = ?", scope)
	}

	var items []models.CategoryOption
	if err := query.Find(&items).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取分类字典失败")
		return
	}

	usageMap := loadCategoryUsageCounts()
	for i := range items {
		if cnt, ok := usageMap[items[i].Scope+"|"+items[i].Name]; ok {
			items[i].UsageCount = cnt
		}
	}

	grouped := map[string][]string{
		"expense": {},
		"income":  {},
		"payment": {},
	}
	for _, it := range items {
		grouped[it.Scope] = append(grouped[it.Scope], it.Name)
	}

	// 兜底：若某个 scope 为空，自动返回默认预置列表
	for sc, defaults := range db.DefaultCategoriesMap {
		if len(grouped[sc]) == 0 {
			grouped[sc] = defaults
		}
	}

	Success(c, gin.H{
		"items":    items,
		"by_scope": grouped,
	})
}

func loadCategoryUsageCounts() map[string]int64 {
	counts := make(map[string]int64)
	type aggRow struct {
		Name string
		Cnt  int64
	}

	if db.DB.Migrator().HasTable(&models.Transaction{}) {
		var expenseRows []aggRow
		db.DB.Model(&models.Transaction{}).
			Select("category AS name, COUNT(*) AS cnt").
			Where("type = ?", "expense").
			Group("category").
			Scan(&expenseRows)
		for _, r := range expenseRows {
			if r.Name != "" {
				counts["expense|"+r.Name] += r.Cnt
			}
		}

		var incomeRows []aggRow
		db.DB.Model(&models.Transaction{}).
			Select("category AS name, COUNT(*) AS cnt").
			Where("type = ?", "income").
			Group("category").
			Scan(&incomeRows)
		for _, r := range incomeRows {
			if r.Name != "" {
				counts["income|"+r.Name] += r.Cnt
			}
		}

		var paymentRows []aggRow
		db.DB.Model(&models.Transaction{}).
			Select("payment_method AS name, COUNT(*) AS cnt").
			Where("payment_method != ''").
			Group("payment_method").
			Scan(&paymentRows)
		for _, r := range paymentRows {
			if r.Name != "" {
				counts["payment|"+r.Name] += r.Cnt
			}
		}
	}

	if db.DB.Migrator().HasTable(&models.InstallmentPlan{}) {
		var planRows []aggRow
		db.DB.Model(&models.InstallmentPlan{}).
			Select("category_name AS name, COUNT(*) AS cnt").
			Where("category_name != ''").
			Group("category_name").
			Scan(&planRows)
		for _, r := range planRows {
			if r.Name != "" {
				counts["expense|"+r.Name] += r.Cnt
			}
		}
	}

	return counts
}

type CreateCategoryRequest struct {
	Scope     string `json:"scope" binding:"required"`
	Name      string `json:"name" binding:"required"`
	GroupName string `json:"group_name"`
	Keywords  string `json:"keywords"`
	SortOrder *int   `json:"sort_order"`
}

// CreateCategory 新增用户自定义分类项
func CreateCategory(c *gin.Context) {
	var req CreateCategoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "分类所属模块与名称不能为空")
		return
	}

	scope := strings.TrimSpace(req.Scope)
	name := strings.TrimSpace(req.Name)
	if !validCategoryScopes[scope] {
		Error(c, http.StatusBadRequest, "不支持的分类模块类型")
		return
	}
	if name == "" {
		Error(c, http.StatusBadRequest, "分类名称不能为空")
		return
	}

	// 检查是否为系统内部保留分类
	if scope == "expense" && db.HiddenSystemExpenseCategories[name] {
		Error(c, http.StatusBadRequest, "该名称为系统内部保留分类")
		return
	}

	// 检查同模块下是否已存在同名分类
	var existing models.CategoryOption
	if err := db.DB.Where("scope = ? AND name = ?", scope, name).First(&existing).Error; err == nil && existing.ID > 0 {
		Error(c, http.StatusBadRequest, "该分类名称已存在")
		return
	}

	sortOrder := 100
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	} else {
		var maxOrder int
		db.DB.Model(&models.CategoryOption{}).Where("scope = ?", scope).Select("COALESCE(MAX(sort_order), 0)").Scan(&maxOrder)
		sortOrder = maxOrder + 10
	}

	item := models.CategoryOption{
		Scope:     scope,
		Name:      name,
		GroupName: strings.TrimSpace(req.GroupName),
		Keywords:  normalizeCategoryKeywords(req.Keywords),
		SortOrder: sortOrder,
		IsBuiltin: false,
	}
	if err := db.DB.Create(&item).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建自定义分类失败")
		return
	}

	Success(c, item)
}

type UpdateCategoryRequest struct {
	Name          string  `json:"name"`
	GroupName     *string `json:"group_name"`
	Keywords      *string `json:"keywords"`
	SortOrder     *int    `json:"sort_order"`
	RenameRecords *bool   `json:"rename_records"`
}

// UpdateCategory 修改分类名称、分组、关键词或排序，并自动同步历史业务记录中的分类名
func UpdateCategory(c *gin.Context) {
	id := c.Param("id")
	var item models.CategoryOption
	if err := db.DB.First(&item, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "该分类项不存在")
		return
	}

	var req UpdateCategoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "无效的请求参数")
		return
	}

	oldName := item.Name
	newName := strings.TrimSpace(req.Name)
	if newName != "" && newName != oldName {
		// 检查重名
		var dup models.CategoryOption
		if err := db.DB.Where("scope = ? AND name = ? AND id != ?", item.Scope, newName, item.ID).First(&dup).Error; err == nil && dup.ID > 0 {
			Error(c, http.StatusBadRequest, "目标分类名称已存在")
			return
		}
		item.Name = newName
	}

	if req.GroupName != nil {
		item.GroupName = strings.TrimSpace(*req.GroupName)
	}
	if req.Keywords != nil {
		item.Keywords = normalizeCategoryKeywords(*req.Keywords)
	}
	if req.SortOrder != nil {
		item.SortOrder = *req.SortOrder
	}

	// 默认同步更新关联的历史数据记录
	shouldRename := true
	if req.RenameRecords != nil {
		shouldRename = *req.RenameRecords
	}
	if err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&item).Error; err != nil {
			return err
		}
		if shouldRename && newName != "" && newName != oldName {
			return cascadeRenameCategoryRecords(tx, item.Scope, oldName, newName)
		}
		return nil
	}); err != nil {
		Error(c, http.StatusInternalServerError, "更新分类失败: "+err.Error())
		return
	}

	item.UsageCount = countCategoryRecordsInUse(item.Scope, item.Name)
	Success(c, item)
}

func cascadeRenameCategoryRecords(tx *gorm.DB, scope, oldName, newName string) error {
	switch scope {
	case "expense":
		if err := tx.Model(&models.Transaction{}).Where("type = 'expense' AND category = ?", oldName).Update("category", newName).Error; err != nil {
			return err
		}
		if tx.Migrator().HasTable(&models.InstallmentPlan{}) {
			if err := tx.Model(&models.InstallmentPlan{}).Where("category_name = ?", oldName).Update("category_name", newName).Error; err != nil {
				return err
			}
		}
		if tx.Migrator().HasTable(&models.CategoryRule{}) {
			if err := tx.Model(&models.CategoryRule{}).Where("tx_type = 'expense' AND category = ?", oldName).Update("category", newName).Error; err != nil {
				return err
			}
		}
		return nil
	case "income":
		if err := tx.Model(&models.Transaction{}).Where("type = 'income' AND category = ?", oldName).Update("category", newName).Error; err != nil {
			return err
		}
		if tx.Migrator().HasTable(&models.CategoryRule{}) {
			if err := tx.Model(&models.CategoryRule{}).Where("tx_type = 'income' AND category = ?", oldName).Update("category", newName).Error; err != nil {
				return err
			}
		}
		return nil
	case "payment":
		return tx.Model(&models.Transaction{}).Where("payment_method = ?", oldName).Update("payment_method", newName).Error
	}
	return nil
}

// countCategoryRecordsInUse 统计某分类名被历史业务记录引用的条数 (BE-28)
func countCategoryRecordsInUse(scope, name string) int64 {
	var total int64
	switch scope {
	case "expense":
		db.DB.Model(&models.Transaction{}).Where("type = 'expense' AND category = ?", name).Count(&total)
		if db.DB.Migrator().HasTable(&models.InstallmentPlan{}) {
			var planCount int64
			db.DB.Model(&models.InstallmentPlan{}).Where("category_name = ?", name).Count(&planCount)
			total += planCount
		}
	case "income":
		db.DB.Model(&models.Transaction{}).Where("type = 'income' AND category = ?", name).Count(&total)
	case "payment":
		db.DB.Model(&models.Transaction{}).Where("payment_method = ?", name).Count(&total)
	}
	return total
}

type DeleteCategoryRequest struct {
	MigrateTo string `json:"migrate_to"`
}

// DeleteCategory 删除分类项 (保证每个模块至少保留 1 个分类；存在历史引用时支持 ?migrate_to= 迁移后删除)
func DeleteCategory(c *gin.Context) {
	id := c.Param("id")
	var item models.CategoryOption
	if err := db.DB.First(&item, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "该分类项不存在")
		return
	}

	if item.IsBuiltin && db.HiddenSystemExpenseCategories[item.Name] {
		Error(c, http.StatusBadRequest, fmt.Sprintf("「%s」为系统内置分类，不可删除", item.Name))
		return
	}

	var count int64
	countQuery := db.DB.Model(&models.CategoryOption{}).Where("scope = ?", item.Scope)
	if item.Scope == "expense" {
		countQuery = countQuery.Where("name NOT IN ?", db.HiddenSystemExpenseNames())
	}
	countQuery.Count(&count)
	if count <= 1 {
		Error(c, http.StatusBadRequest, "每个分类模块至少需要保留 1 个分类项")
		return
	}

	migrateTo := strings.TrimSpace(c.Query("migrate_to"))
	if migrateTo == "" && c.Request.Body != nil {
		rawBytes, _ := io.ReadAll(io.LimitReader(c.Request.Body, 4096))
		if len(strings.TrimSpace(string(rawBytes))) > 0 {
			var bodyReq DeleteCategoryRequest
			if err := json.Unmarshal(rawBytes, &bodyReq); err == nil {
				migrateTo = strings.TrimSpace(bodyReq.MigrateTo)
			}
		}
	}

	if migrateTo != "" {
		if migrateTo == item.Name {
			Error(c, http.StatusBadRequest, "目标迁移分类不能与当前删除分类相同")
			return
		}
		var target models.CategoryOption
		if err := db.DB.Where("scope = ? AND name = ?", item.Scope, migrateTo).First(&target).Error; err != nil || target.ID == 0 {
			Error(c, http.StatusBadRequest, "目标迁移分类不存在于当前模块字典")
			return
		}
	}

	// 仍被历史业务记录引用的分类：若未指定 migrate_to 则拒绝删除，否则在事务内迁移后删除
	used := countCategoryRecordsInUse(item.Scope, item.Name)
	if used > 0 && migrateTo == "" {
		Error(c, http.StatusBadRequest,
			fmt.Sprintf("「%s」仍被 %d 条历史记录引用，暂无法删除", item.Name, used))
		return
	}

	if err := db.DB.Transaction(func(tx *gorm.DB) error {
		if migrateTo != "" {
			if err := cascadeRenameCategoryRecords(tx, item.Scope, item.Name, migrateTo); err != nil {
				return err
			}
		}
		return tx.Delete(&item).Error
	}); err != nil {
		Error(c, http.StatusInternalServerError, "删除分类失败: "+err.Error())
		return
	}

	Success(c, gin.H{"id": item.ID, "deleted": true, "migrated_to": migrateTo})
}
