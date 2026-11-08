package handlers

import (
	"sync/atomic"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 增量同步墓碑（P2-3 方案 1）。
//
// purge 是物理删除：行从表里消失后，`updated_at/deleted_at >= since` 的增量
// 查询再也看不到它。离线超过「软删 → purge」窗口的客户端会永久保留一条服务端
// 已不存在的记录，并且后续任何全量对账都可能把它重新插回本地。
//
// 解决方式：purge 的同一事务里写一条最小墓碑（类型 + 原 id + 删除时刻）。
// 五类列表接口在带 since 的增量请求里把墓碑合成回对应模型的 JSON 行，
// 客户端现有的 `deleted_at` 合并逻辑即可删除本地行，无需新增接口或协议。
const (
	TombstoneEntityEntry       = "entry"
	TombstoneEntityTransaction = "transaction"
	TombstoneEntityProject     = "project"
	TombstoneEntityWish        = "wish"
	TombstoneEntityFootprint   = "footprint"
)

// lastTombstoneNano 保证同一进程内写出的墓碑时间严格递增。
//
// 为什么需要：增量游标是秒级 RFC3339，且过滤条件是 `deleted_at >= since`。
// 若一次级联 purge 写出成百上千条同一时刻的墓碑，而响应又按页截断，
// 客户端永远只能看到同一批「最老」的墓碑，游标无法向前推进。
// 这里用纳秒级单调时间让每条墓碑都有唯一顺序；响应不做截断时它只是
// 让排序稳定，不依赖客户端解析亚秒精度。
var lastTombstoneNano int64

func nextTombstoneTime() time.Time {
	now := time.Now().In(config.BeijingLocation).UnixNano()
	for {
		prev := atomic.LoadInt64(&lastTombstoneNano)
		next := now
		if next <= prev {
			next = prev + 1
		}
		if atomic.CompareAndSwapInt64(&lastTombstoneNano, prev, next) {
			return time.Unix(0, next).In(config.BeijingLocation)
		}
	}
}

// recordSyncTombstone 在调用方事务内写入一条墓碑。
// 唯一索引 (entity_type, entity_id) 让重复 purge / 历史重放保持幂等；
// 冲突时用更新的删除时刻覆盖，保证「最后一次物理删除」仍能被增量同步看到。
func recordSyncTombstone(tx *gorm.DB, entityType string, entityID uint) error {
	if tx == nil || entityID == 0 {
		return nil
	}
	row := models.SyncTombstone{
		EntityType: entityType,
		EntityID:   entityID,
		DeletedAt:  nextTombstoneTime(),
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "entity_type"}, {Name: "entity_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"deleted_at"}),
	}).Create(&row).Error
}

func recordSyncTombstones(tx *gorm.DB, entityType string, entityIDs []uint) error {
	for _, id := range entityIDs {
		if err := recordSyncTombstone(tx, entityType, id); err != nil {
			return err
		}
	}
	return nil
}

// syncTombstonesSince 读取某类实体在 since 之后（含等号）被物理删除的墓碑。
//
// 刻意**不截断**：游标是秒级精度，若按页截断，同一秒内的剩余墓碑无法靠
// 下一次 since 继续翻页（`>=` 会把已返回的那批再次带回），会永久漏删。
// purge 是低频管理操作，墓碑总量远小于正常数据量，一次全量返回是可接受的。
func syncTombstonesSince(entityType, sinceStr string) ([]models.SyncTombstone, error) {
	if sinceStr == "" {
		return nil, nil
	}
	sinceTime, err := ParseSyncSinceTime(sinceStr)
	if err != nil {
		return nil, err
	}
	var rows []models.SyncTombstone
	if err := db.DB.
		Where("entity_type = ? AND deleted_at >= ?", entityType, sinceTime).
		Order("deleted_at ASC, entity_id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func injectEntryTombstones(dst *[]models.LifeEntry, sinceStr string) error {
	rows, err := syncTombstonesSince(TombstoneEntityEntry, sinceStr)
	if err != nil || len(rows) == 0 {
		return err
	}
	existing := make(map[uint]struct{}, len(*dst))
	for _, item := range *dst {
		existing[item.ID] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := existing[row.EntityID]; ok {
			continue
		}
		*dst = append(*dst, models.LifeEntry{
			ID:        row.EntityID,
			CreatedAt: row.DeletedAt,
			UpdatedAt: row.DeletedAt,
			DeletedAt: gorm.DeletedAt{Time: row.DeletedAt, Valid: true},
		})
	}
	return nil
}

func injectTransactionTombstones(dst *[]models.Transaction, sinceStr string) error {
	rows, err := syncTombstonesSince(TombstoneEntityTransaction, sinceStr)
	if err != nil || len(rows) == 0 {
		return err
	}
	existing := make(map[uint]struct{}, len(*dst))
	for _, item := range *dst {
		existing[item.ID] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := existing[row.EntityID]; ok {
			continue
		}
		*dst = append(*dst, models.Transaction{
			ID:        row.EntityID,
			CreatedAt: row.DeletedAt,
			UpdatedAt: row.DeletedAt,
			DeletedAt: gorm.DeletedAt{Time: row.DeletedAt, Valid: true},
		})
	}
	return nil
}

func injectProjectTombstones(dst *[]models.Project, sinceStr string) error {
	rows, err := syncTombstonesSince(TombstoneEntityProject, sinceStr)
	if err != nil || len(rows) == 0 {
		return err
	}
	existing := make(map[uint]struct{}, len(*dst))
	for _, item := range *dst {
		existing[item.ID] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := existing[row.EntityID]; ok {
			continue
		}
		*dst = append(*dst, models.Project{
			ID:        row.EntityID,
			CreatedAt: row.DeletedAt,
			UpdatedAt: row.DeletedAt,
			DeletedAt: gorm.DeletedAt{Time: row.DeletedAt, Valid: true},
		})
	}
	return nil
}

func injectWishTombstones(dst *[]models.Wish, sinceStr string) error {
	rows, err := syncTombstonesSince(TombstoneEntityWish, sinceStr)
	if err != nil || len(rows) == 0 {
		return err
	}
	existing := make(map[uint]struct{}, len(*dst))
	for _, item := range *dst {
		existing[item.ID] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := existing[row.EntityID]; ok {
			continue
		}
		*dst = append(*dst, models.Wish{
			ID:        row.EntityID,
			CreatedAt: row.DeletedAt,
			UpdatedAt: row.DeletedAt,
			DeletedAt: gorm.DeletedAt{Time: row.DeletedAt, Valid: true},
		})
	}
	return nil
}

func injectFootprintTombstones(dst *[]models.Footprint, sinceStr string) error {
	rows, err := syncTombstonesSince(TombstoneEntityFootprint, sinceStr)
	if err != nil || len(rows) == 0 {
		return err
	}
	existing := make(map[uint]struct{}, len(*dst))
	for _, item := range *dst {
		existing[item.ID] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := existing[row.EntityID]; ok {
			continue
		}
		*dst = append(*dst, models.Footprint{
			ID:        row.EntityID,
			CreatedAt: row.DeletedAt,
			UpdatedAt: row.DeletedAt,
			DeletedAt: gorm.DeletedAt{Time: row.DeletedAt, Valid: true},
		})
	}
	return nil
}
