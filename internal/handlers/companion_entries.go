package handlers

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"loose-leaf/internal/models"
)

// 伴生一级记录（Companion Entry）同步工具
//
// 约定（Record-First Architecture）：心愿在时间流里对应一条一级散页。
// 本文件负责在心愿被修改或删除时同步维护这条伴生散页。
// 所有操作都在调用方的事务（tx）内执行，保证原子生效。
const (
	// 伴生散页的标题前缀
	companionWishPlantedTitlePrefix   = "许下心愿 · "
	companionWishFulfilledTitlePrefix = "心愿达成 · "
)

// buildWishCompanionContent 生成心愿伴生散页的正文
func buildWishCompanionContent(wish models.Wish) string {
	if content := strings.TrimSpace(wish.Note); content != "" {
		return content
	}
	if spec := strings.TrimSpace(wish.Spec); spec != "" {
		return fmt.Sprintf("许下心愿「%s」（%s）。", wish.Title, spec)
	}
	return fmt.Sprintf("许下心愿「%s」，期待日后圆满归册。", wish.Title)
}

// syncWishCompanionEntry 同步「许下心愿」那条伴生散页的标题 / 正文。
//
// 注意：心愿达成时会另外新建一条「心愿达成 · xxx」的伴生散页，
// 本函数只更新「许下心愿 · xxx」那一条，不会动达成记录。
func syncWishCompanionEntry(tx *gorm.DB, wish models.Wish, oldTitle string) error {
	oldCompanionTitle := companionWishPlantedTitlePrefix + oldTitle
	newCompanionTitle := companionWishPlantedTitlePrefix + wish.Title
	return tx.Model(&models.LifeEntry{}).
		Where("wish_id = ? AND title = ?", wish.ID, oldCompanionTitle).
		Updates(map[string]any{
			"title":      newCompanionTitle,
			"content":    buildWishCompanionContent(wish),
			"updated_at": time.Now(),
		}).Error
}

// deleteWishCompanionEntries 删除心愿的全部伴生散页（软删除）
func deleteWishCompanionEntries(tx *gorm.DB, wishID uint) error {
	return tx.Where("wish_id = ?", wishID).
		Delete(&models.LifeEntry{}).Error
}
