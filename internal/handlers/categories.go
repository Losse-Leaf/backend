package handlers

import (
	"fmt"
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
	"gear":    true,
	"payment": true,
	"project": true,
}

// GetCategories 获取分类字典列表 (支持 ?scope= 过滤，同时返回列表和按 scope 分组的字典)
func GetCategories(c *gin.Context) {
	scope := strings.TrimSpace(c.Query("scope"))
	query := db.DB.Model(&models.CategoryOption{}).Order("scope ASC, sort_order ASC, id ASC")
	if scope != "" {
		query = query.Where("scope = ?", scope)
	}

	var items []models.CategoryOption
	if err := query.Find(&items).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取分类字典失败")
		return
	}

	grouped := map[string][]string{
		"expense": {},
		"income":  {},
		"gear":    {},
		"payment": {},
		"project": {},
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

type CreateCategoryRequest struct {
	Scope     string `json:"scope" binding:"required"`
	Name      string `json:"name" binding:"required"`
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
	Name          string `json:"name"`
	SortOrder     *int   `json:"sort_order"`
	RenameRecords *bool  `json:"rename_records"`
}

// UpdateCategory 修改分类名称或排序，并自动同步历史业务记录中的分类名
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

	Success(c, item)
}

func cascadeRenameCategoryRecords(tx *gorm.DB, scope, oldName, newName string) error {
	switch scope {
	case "expense":
		return tx.Model(&models.Transaction{}).Where("type = 'expense' AND category = ?", oldName).Update("category", newName).Error
	case "income":
		return tx.Model(&models.Transaction{}).Where("type = 'income' AND category = ?", oldName).Update("category", newName).Error
	case "payment":
		return tx.Model(&models.Transaction{}).Where("payment_method = ?", oldName).Update("payment_method", newName).Error
	case "project":
		return tx.Model(&models.Project{}).Where("category = ?", oldName).Update("category", newName).Error
	case "gear":
		// EDCGear 同行 JSON 列读-改-写须串行化 (BE-34)
		profileGearMu.Lock()
		defer profileGearMu.Unlock()
		var profile models.CuratorProfile
		if err := tx.First(&profile).Error; err != nil {
			return err
		}
		list := loadProfileGear(&profile)
		changed := false
		for i := range list {
			if list[i].Category == oldName {
				list[i].Category = newName
				changed = true
			}
		}
		if changed {
			return saveProfileGearWith(tx, &profile, list)
		}
	}
	return nil
}

// countCategoryRecordsInUse 统计某分类名被历史业务记录引用的条数 (BE-28)
func countCategoryRecordsInUse(scope, name string) int64 {
	var total int64
	switch scope {
	case "expense":
		db.DB.Model(&models.Transaction{}).Where("type = 'expense' AND category = ?", name).Count(&total)
	case "income":
		db.DB.Model(&models.Transaction{}).Where("type = 'income' AND category = ?", name).Count(&total)
	case "payment":
		db.DB.Model(&models.Transaction{}).Where("payment_method = ?", name).Count(&total)
	case "project":
		db.DB.Model(&models.Project{}).Where("category = ?", name).Count(&total)
	case "gear":
		profileGearMu.Lock()
		defer profileGearMu.Unlock()
		var profile models.CuratorProfile
		if err := db.DB.First(&profile).Error; err == nil {
			for _, g := range loadProfileGear(&profile) {
				if g.Category == name {
					total++
				}
			}
		}
	}
	return total
}

// DeleteCategory 删除分类项 (保证每个模块至少保留 1 个分类；存在历史引用时拒绝删除)
func DeleteCategory(c *gin.Context) {
	id := c.Param("id")
	var item models.CategoryOption
	if err := db.DB.First(&item, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "该分类项不存在")
		return
	}

	var count int64
	db.DB.Model(&models.CategoryOption{}).Where("scope = ?", item.Scope).Count(&count)
	if count <= 1 {
		Error(c, http.StatusBadRequest, "每个分类模块至少需要保留 1 个分类项")
		return
	}

	// 仍被历史业务记录引用的分类不可删除，否则旧记录将残留孤儿分类名 (BE-28)
	if used := countCategoryRecordsInUse(item.Scope, item.Name); used > 0 {
		Error(c, http.StatusBadRequest,
			fmt.Sprintf("「%s」仍被 %d 条历史记录引用，暂无法删除", item.Name, used))
		return
	}

	if err := db.DB.Delete(&item).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除分类失败")
		return
	}

	Success(c, gin.H{"id": item.ID, "deleted": true})
}

type ResetCategoriesRequest struct {
	Scope string `json:"scope"`
}

// ResetCategories 恢复指定或全部模块的预置默认分类项
func ResetCategories(c *gin.Context) {
	var req ResetCategoriesRequest
	_ = c.ShouldBindJSON(&req)
	scope := strings.TrimSpace(req.Scope)
	if scope != "" && !validCategoryScopes[scope] {
		Error(c, http.StatusBadRequest, "不支持的分类模块类型")
		return
	}

	db.SeedDefaultCategories(db.DB, scope, true)
	GetCategories(c)
}
