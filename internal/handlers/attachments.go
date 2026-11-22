package handlers

import (
	"errors"
	"fmt"
	"maps"

	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"gorm.io/gorm"
)

// populateAttachmentURLs 填充附件的动态临时预签名访问 URL 与缩略图 URL，并保证返回非 nil 切片。
func populateAttachmentURLs(atts []models.Attachment) []models.Attachment {
	if atts == nil {
		return []models.Attachment{}
	}
	for i := range atts {
		atts[i].URL = storage.SignResourceURL(atts[i].ObjectKey)
		atts[i].ThumbnailURL = ThumbnailURLFor(atts[i].ObjectKey, 0)
	}
	return atts
}

// errAttachmentClaimConflict 附件跨实体抢占（发现 15）：
// 待绑定附件已归属其他实体（entry / wish），拒绝绑定。
var errAttachmentClaimConflict = errors.New("附件已绑定到其他实体")

func claimAttachments(dbTx *gorm.DB, ids []uint, targets map[string]any) error {
	if len(ids) == 0 || dbTx == nil {
		return nil
	}
	allowedTargets := make(map[string]any, len(targets)+1)
	for key, value := range targets {
		allowedTargets[key] = value
	}
	if entryID, ok := targets["entry_id"].(uint); ok && entryID > 0 {
		var entry models.LifeEntry
		if err := dbTx.Select("id", "wish_id").First(&entry, entryID).Error; err != nil {
			return err
		}
		if _, explicit := targets["wish_id"]; !explicit && entry.WishID != nil {
			allowedTargets["wish_id"] = *entry.WishID
		}
	}
	var atts []models.Attachment
	if err := dbTx.Where("id IN ?", ids).Find(&atts).Error; err != nil {
		return err
	}
	byID := make(map[uint]models.Attachment, len(atts))
	for _, att := range atts {
		byID[att.ID] = att
	}
	for _, id := range ids {
		att, ok := byID[id]
		if !ok {
			return fmt.Errorf("附件 #%d 不存在或已被清理", id)
		}
		bindingTargets := allowedTargets
		if att.EntryID != nil && targets["entry_id"] == *att.EntryID {
			bindingTargets = maps.Clone(allowedTargets)
			if _, explicit := targets["wish_id"]; !explicit && att.WishID != nil {
				bindingTargets["wish_id"] = *att.WishID
			}
		}
		if err := assertAttachmentBindable(att, bindingTargets); err != nil {
			return err
		}
	}
	return dbTx.Model(&models.Attachment{}).Where("id IN ?", ids).Updates(targets).Error
}

// assertAttachmentBindable 单个附件的归属校验：两个归属列各自为 NULL，
// 或恰好等于本次要写入的目标值，否则视为抢占冲突。
func assertAttachmentBindable(att models.Attachment, targets map[string]any) error {
	ownerNames := map[string]string{
		"entry_id": "生活散页",
		"wish_id":  "心愿",
	}
	check := func(col string, cur *uint) error {
		if want, has := targets[col]; has {
			wantID, _ := want.(uint)
			if cur != nil && *cur != wantID {
				return fmt.Errorf("%w：附件 #%d 已绑定到其他%s", errAttachmentClaimConflict, att.ID, ownerNames[col])
			}
			return nil
		}
		if cur != nil {
			return fmt.Errorf("%w：附件 #%d 已绑定到其他%s", errAttachmentClaimConflict, att.ID, ownerNames[col])
		}
		return nil
	}
	if err := check("entry_id", att.EntryID); err != nil {
		return err
	}
	return check("wish_id", att.WishID)
}

// releaseEntryAttachments 删除散页（物理 purge 或软删）时处理其附件归属，防止悬挂引用。
func releaseEntryAttachments(dbTx *gorm.DB, entryID uint, purge bool) (keysToDelete []string, err error) {
	var atts []models.Attachment
	if err := dbTx.Where("entry_id = ?", entryID).Find(&atts).Error; err != nil {
		return nil, err
	}
	for i := range atts {
		att := atts[i]
		if purge && att.WishID == nil {
			if att.ObjectKey != "" {
				keysToDelete = append(keysToDelete, att.ObjectKey)
			}
			if err := dbTx.Unscoped().Delete(&models.Attachment{}, att.ID).Error; err != nil {
				return keysToDelete, err
			}
			continue
		}
		if err := dbTx.Model(&models.Attachment{}).Where("id = ?", att.ID).Update("entry_id", nil).Error; err != nil {
			return keysToDelete, err
		}
	}
	return keysToDelete, nil
}
