package handlers

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"loose-leaf/internal/config"
	"loose-leaf/internal/models"
)

// 伴生一级记录（Companion Entry）同步工具
//
// 约定（Record-First Architecture）：二级对象（心愿 / 造物 / 器物 / 足迹）在**时间流**里
// 都有一条对应的一级散页。本文件负责在二级对象被**创建 / 修改 / 删除**时，同步维护这条伴生散页，
// 避免出现「心愿改了标题、时间流里还是旧标题」或「心愿删了、时间流留下孤儿记录」。
//
// 所有操作都在调用方的事务（tx）内执行，保证与二级对象的写入原子生效。
//
// 定位策略：伴生散页通过 **旧标题精确匹配** 定位（而不是只按外键匹配），
// 因为同一个二级对象可能对应多条伴生散页 —— 例如心愿「许下」一条、「达成」又一条，
// 造物「立项」一条、每次「进展」又各一条。只按外键匹配会误伤历史记录。

const (
	// 伴生散页的标题前缀
	companionWishPlantedTitlePrefix   = "许下心愿 · "
	companionWishFulfilledTitlePrefix = "心愿达成 · "
	companionGearTitlePrefix          = "添置器物 · "
)

// buildWishCompanionContent 生成心愿伴生散页的正文
func buildWishCompanionContent(wish models.Wish) string {
	if content := strings.TrimSpace(wish.Note); content != "" {
		return content
	}
	if spec := strings.TrimSpace(wish.Spec); spec != "" {
		return fmt.Sprintf("许下心愿「%s」（%s）。", wish.Title, spec)
	}
	return fmt.Sprintf("许下心愿「%s」，期待日后圆满归航。", wish.Title)
}

// syncWishCompanionEntry 同步「许下心愿」那条伴生散页的标题 / 正文 / 地点。
//
// 注意：心愿达成时会**另外**新建一条「心愿达成 · xxx」的伴生散页（那是新的时刻，值得留痕），
// 本函数只更新「许下心愿 · xxx」那一条，不会动达成记录。
func syncWishCompanionEntry(tx *gorm.DB, wish models.Wish, oldTitle string) error {
	oldCompanionTitle := companionWishPlantedTitlePrefix + oldTitle
	newCompanionTitle := companionWishPlantedTitlePrefix + wish.Title
	return tx.Model(&models.LifeEntry{}).
		Where("wish_id = ? AND title = ?", wish.ID, oldCompanionTitle).
		Updates(map[string]any{
			"title":      newCompanionTitle,
			"content":    buildWishCompanionContent(wish),
			"location":   wish.CityName,
			"city_code":  wish.CityCode,
			"city_name":  wish.CityName,
			"updated_at": time.Now(),
		}).Error
}

// deleteWishCompanionEntries 删除心愿的全部伴生散页（软删除）
func deleteWishCompanionEntries(tx *gorm.DB, wishID uint) error {
	return tx.Where("wish_id = ?", wishID).
		Delete(&models.LifeEntry{}).Error
}

// buildProjectCompanionContent 生成造物伴生散页的正文
func buildProjectCompanionContent(p models.Project) string {
	if content := strings.TrimSpace(p.Description); content != "" {
		return content
	}
	if content := strings.TrimSpace(p.Story); content != "" {
		return content
	}
	if content := strings.TrimSpace(p.Subtitle); content != "" {
		return content
	}
	return fmt.Sprintf("立项开启造物项目「%s」。", p.Title)
}

// createProjectCompanionEntry 立项时伴生创建一条一级散页
func createProjectCompanionEntry(tx *gorm.DB, p models.Project) (models.LifeEntry, error) {
	companionEntry := models.LifeEntry{
		Type:       "project",
		Title:      p.Title,
		Content:    buildProjectCompanionContent(p),
		OccurredAt: time.Now().In(config.BeijingLocation),
		ProjectID:  &p.ID,
	}
	if err := tx.Create(&companionEntry).Error; err != nil {
		return companionEntry, err
	}
	return companionEntry, nil
}

// syncProjectCompanionEntry 同步「立项」那条伴生散页（标题 / 正文）。
// 进展手记与状态演进产生的伴生散页不在此处更新 —— 它们是独立的历史时刻。
func syncProjectCompanionEntry(tx *gorm.DB, p models.Project, oldTitle string) error {
	return tx.Model(&models.LifeEntry{}).
		Where("project_id = ? AND title = ?", p.ID, oldTitle).
		Updates(map[string]any{
			"title":      p.Title,
			"content":    buildProjectCompanionContent(p),
			"updated_at": time.Now(),
		}).Error
}

// deleteProjectCompanionEntries 删除造物的全部伴生散页（软删除）
func deleteProjectCompanionEntries(tx *gorm.DB, projectID uint) error {
	return tx.Where("project_id = ?", projectID).
		Delete(&models.LifeEntry{}).Error
}

// buildGearCompanionContent 生成器物伴生散页的正文
func buildGearCompanionContent(gear models.GearItem) string {
	if content := strings.TrimSpace(gear.Note); content != "" {
		return content
	}
	return fmt.Sprintf("添置随身器物「%s」（%s · %s）。", gear.Name, gear.Category, gear.Spec)
}

// createGearCompanionEntry 添置器物时伴生创建一条一级散页
func createGearCompanionEntry(tx *gorm.DB, gear models.GearItem, occurredAt time.Time) (models.LifeEntry, error) {
	gearID := gear.ID
	companionEntry := models.LifeEntry{
		Type:       "life",
		Title:      companionGearTitlePrefix + gear.Name,
		Content:    buildGearCompanionContent(gear),
		OccurredAt: occurredAt,
		GearID:     &gearID,
	}
	if err := tx.Create(&companionEntry).Error; err != nil {
		return companionEntry, err
	}
	return companionEntry, nil
}

// syncGearCompanionEntry 同步器物伴生散页（标题 / 正文）
func syncGearCompanionEntry(tx *gorm.DB, gear models.GearItem, oldName string) error {
	oldCompanionTitle := companionGearTitlePrefix + oldName
	newCompanionTitle := companionGearTitlePrefix + gear.Name
	return tx.Model(&models.LifeEntry{}).
		Where("gear_id = ? AND title = ?", gear.ID, oldCompanionTitle).
		Updates(map[string]any{
			"title":      newCompanionTitle,
			"content":    buildGearCompanionContent(gear),
			"updated_at": time.Now(),
		}).Error
}

// deleteGearCompanionEntries 删除器物的伴生散页（软删除）
func deleteGearCompanionEntries(tx *gorm.DB, gearID string) error {
	return tx.Where("gear_id = ?", gearID).
		Delete(&models.LifeEntry{}).Error
}
