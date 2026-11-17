package handlers

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
)

// footprintTripWindow 计算一次旅程足迹的起止日期窗口（含容差天数）
func footprintTripWindow(f *models.Footprint, padDays int) (time.Time, time.Time) {
	start := time.Date(f.VisitedAt.Year(), f.VisitedAt.Month(), f.VisitedAt.Day(), 0, 0, 0, 0, f.VisitedAt.Location()).AddDate(0, 0, -padDays)
	endBase := f.VisitedAt
	if f.DepartedAt != nil && !f.DepartedAt.IsZero() && f.DepartedAt.After(endBase) {
		endBase = *f.DepartedAt
	}
	end := time.Date(endBase.Year(), endBase.Month(), endBase.Day(), 23, 59, 59, 0, endBase.Location()).AddDate(0, 0, padDays)
	return start, end
}

// autoLinkTransactionFootprint 自动对齐记账记录的城市与所属旅程足迹 (方案 A + B 结合)
func autoLinkTransactionFootprint(tx *models.Transaction) {
	if tx == nil {
		return
	}
	if tx.FootprintID != nil && *tx.FootprintID > 0 {
		if tx.CityCode == "" {
			var fp models.Footprint
			if err := db.DB.First(&fp, *tx.FootprintID).Error; err == nil && fp.ID > 0 {
				tx.CityCode = fp.CityCode
			}
		}
		return
	}
	if fpID, ok := bestFootprintFor(tx); ok {
		idCopy := fpID
		tx.FootprintID = &idCopy
	}
}

// footprintLinkPadDays 账目自动挂载到旅程时的日期容差（前后各放宽几天）
const footprintLinkPadDays = 2

// bestFootprintFor 按「同城 + 旅程窗口（± 容差）+ 取最近的一趟」的规则，
// 找出一条账目应当归属的旅程。
func bestFootprintFor(tx *models.Transaction) (uint, bool) {
	return bestFootprintForDB(db.DB, tx)
}

// bestFootprintForDB 与 bestFootprintFor 同一套规则，但允许在事务连接上查询，
// 供启动回填的「每批 500 条一个事务」复用。
func bestFootprintForDB(database *gorm.DB, tx *models.Transaction) (uint, bool) {
	if tx == nil || tx.CityCode == "" || database == nil {
		return 0, false
	}
	occ := tx.OccurredAt
	if occ.IsZero() {
		occ = time.Now()
	}
	var cityFps []models.Footprint
	database.Where("city_code = ?", tx.CityCode).Order("visited_at DESC").Find(&cityFps)
	for _, cand := range cityFps {
		wStart, wEnd := footprintTripWindow(&cand, footprintLinkPadDays)
		if !occ.Before(wStart) && !occ.After(wEnd) {
			return cand.ID, true
		}
	}
	return 0, false
}

// linkUnlinkedTransactions 把所有「未挂载、且按自动挂载规则能找到归属」的账目补挂上去。
func linkUnlinkedTransactions(onlyCityCode string) int {
	linked, _, err := linkUnlinkedTransactionsBatched(onlyCityCode, footprintBackfillBatchSize)
	if err != nil {
		log.Printf("[Footprint Backfill] 账目挂载失败: %v", err)
	}
	return linked
}

// linkUnlinkedTransactionsBatched 按 id 游标分批处理，每批在一个事务内写入。
func linkUnlinkedTransactionsBatched(onlyCityCode string, batchSize int) (linked int, processed int, err error) {
	if batchSize <= 0 {
		batchSize = footprintBackfillBatchSize
	}
	var lastID uint
	for {
		q := db.DB.Where("(footprint_id IS NULL OR footprint_id = 0) AND city_code != '' AND id > ?", lastID)
		if onlyCityCode != "" {
			q = q.Where("city_code = ?", onlyCityCode)
		}
		var txs []models.Transaction
		if err := q.Order("id ASC").Limit(batchSize).Find(&txs).Error; err != nil {
			return linked, processed, err
		}
		if len(txs) == 0 {
			break
		}
		lastID = txs[len(txs)-1].ID
		processed += len(txs)

		if err := db.DB.Transaction(func(tx *gorm.DB) error {
			affectedFPs := make(map[uint]struct{})
			for i := range txs {
				if txs[i].FootprintID != nil && *txs[i].FootprintID > 0 {
					continue
				}
				fpID, ok := bestFootprintForDB(tx, &txs[i])
				if !ok {
					continue
				}
				res := tx.Model(&models.Transaction{}).
					Where("id = ? AND (footprint_id IS NULL OR footprint_id = 0)", txs[i].ID).
					Update("footprint_id", fpID)
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected > 0 {
					affectedFPs[fpID] = struct{}{}
				}
				linked += int(res.RowsAffected)
			}
			for fpID := range affectedFPs {
				touchFootprintTimestamps(tx, fpID)
			}
			return nil
		}); err != nil {
			return linked, processed, err
		}
	}
	return linked, processed, nil
}

// EnsureFootprintTransactionsLinked 启动时的全量回填。
func EnsureFootprintTransactionsLinked() {
	if db.DB == nil {
		return
	}
	start := time.Now()
	syncFootprintLock.Lock()
	defer syncFootprintLock.Unlock()

	linked, processed, err := linkUnlinkedTransactionsBatched("", footprintBackfillBatchSize)
	if err != nil {
		log.Printf("[Footprint Backfill] 账目→旅程回填失败（已处理 %d 笔，挂载 %d 笔）: %v", processed, linked, err)
		return
	}
	log.Printf("[Footprint Backfill] 账目→旅程回填完成：扫描 %d 笔，挂载 %d 笔，耗时 %s",
		processed, linked, time.Since(start).Round(time.Millisecond))
}

// touchFootprintTimestamps 在账目变动后推进所属旅程的 updated_at。
func touchFootprintTimestamps(database *gorm.DB, ids ...uint) {
	if database == nil {
		return
	}
	now := config.NowInBeijing()
	for _, id := range ids {
		if id == 0 {
			continue
		}
		database.Model(&models.Footprint{}).Where("id = ?", id).UpdateColumn("updated_at", now)
	}
}

// expandFootprintDateRange 当新的动态发生日期超出当前旅程起止范围（跨天）时自动扩展旅程起止日期
func expandFootprintDateRange(fp *models.Footprint, t time.Time) bool {
	if fp == nil || t.IsZero() {
		return false
	}
	changed := false
	startDay := time.Date(fp.VisitedAt.Year(), fp.VisitedAt.Month(), fp.VisitedAt.Day(), 0, 0, 0, 0, fp.VisitedAt.Location())
	endBase := fp.VisitedAt
	if fp.DepartedAt != nil && !fp.DepartedAt.IsZero() && fp.DepartedAt.After(endBase) {
		endBase = *fp.DepartedAt
	}
	endDay := time.Date(endBase.Year(), endBase.Month(), endBase.Day(), 0, 0, 0, 0, endBase.Location())
	curDay := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())

	if curDay.Before(startDay) {
		if fp.DepartedAt == nil || fp.DepartedAt.IsZero() {
			oldStart := fp.VisitedAt
			fp.DepartedAt = &oldStart
		}
		fp.VisitedAt = t
		changed = true
	} else if curDay.After(endDay) {
		dep := t
		fp.DepartedAt = &dep
		changed = true
	}
	return changed
}

// syncFootprintFromLifeEntry 主流自然下渗与旅程智能归并 (方案 A + B 结合)
func syncFootprintFromLifeEntry(entry *models.LifeEntry, attachmentIDs []uint, dbTx ...*gorm.DB) error {
	if entry == nil || entry.ID == 0 {
		return nil
	}
	targetDB := db.DB
	if len(dbTx) > 0 && dbTx[0] != nil {
		targetDB = dbTx[0]
	}

	cityCode := strings.TrimSpace(entry.CityCode)
	if cityCode == "" || entry.Type == "wish" {
		return nil
	}

	cityName := strings.TrimSpace(entry.CityName)
	if cityName == "" {
		cityName = strings.TrimSpace(entry.Location)
	}
	if cityName == "" {
		cityName = cityCode
	}

	noteText := strings.TrimSpace(entry.Content)
	if noteText == "" {
		noteText = strings.TrimSpace(entry.Title)
	}

	visitedAt := entry.OccurredAt
	if visitedAt.IsZero() {
		visitedAt = time.Now()
	}

	var fp models.Footprint

	// 1. 优先检查 entry.FootprintID 显式绑定的旅程 (方案 A)
	if entry.FootprintID != nil && *entry.FootprintID > 0 {
		_ = targetDB.Where("id = ?", *entry.FootprintID).Limit(1).Find(&fp).Error
	}

	// 2. 检查是否已有直接以该 entry 作为首条伴生的 Footprint
	if fp.ID == 0 {
		_ = targetDB.Where("life_entry_id = ?", entry.ID).Limit(1).Find(&fp).Error
	}

	// 3. 同城连续多日动态自动归并为一趟旅程 (方案 B: ±7 天窗口)
	if fp.ID == 0 {
		var cityFps []models.Footprint
		targetDB.Where("city_code = ?", cityCode).Order("visited_at DESC").Find(&cityFps)
		for _, cand := range cityFps {
			wStart, wEnd := footprintTripWindow(&cand, 7)
			if !visitedAt.Before(wStart) && !visitedAt.After(wEnd) {
				fp = cand
				break
			}
		}
	}

	if fp.ID > 0 {
		needSave := false
		if fp.LifeEntryID != nil && *fp.LifeEntryID == entry.ID {
			fp.CityCode = cityCode
			fp.CityName = cityName
			if fp.DepartedAt == nil {
				fp.VisitedAt = visitedAt
			} else {
				expandFootprintDateRange(&fp, visitedAt)
			}
			if noteText != "" {
				fp.Note = noteText
			}
			if fp.Title == "" && strings.TrimSpace(entry.Title) != "" {
				fp.Title = strings.TrimSpace(entry.Title)
			}
			needSave = true
		} else {
			if expandFootprintDateRange(&fp, visitedAt) {
				needSave = true
			}
		}
		if needSave {
			if err := targetDB.Save(&fp).Error; err != nil {
				return fmt.Errorf("更新足迹失败 entry=%d footprint=%d: %w", entry.ID, fp.ID, err)
			}
		}
	} else {
		fp = models.Footprint{
			CityCode:    cityCode,
			CityName:    cityName,
			Title:       strings.TrimSpace(entry.Title),
			VisitedAt:   visitedAt,
			Note:        noteText,
			LifeEntryID: &entry.ID,
			WishID:      entry.WishID,
			IsPrivate:   entry.IsPrivate,
		}
		if err := targetDB.Create(&fp).Error; err != nil {
			return fmt.Errorf("创建足迹失败 entry=%d: %w", entry.ID, err)
		}
	}

	if fp.ID > 0 {
		if entry.FootprintID == nil || *entry.FootprintID != fp.ID {
			fpID := fp.ID
			entry.FootprintID = &fpID
			if err := targetDB.Model(&models.LifeEntry{}).Where("id = ?", entry.ID).Update("footprint_id", fp.ID).Error; err != nil {
				return fmt.Errorf("回填散页 footprint_id 失败 entry=%d: %w", entry.ID, err)
			}
		}
		if len(attachmentIDs) > 0 {
			if err := claimAttachments(targetDB, attachmentIDs, map[string]any{
				"footprint_id": fp.ID,
				"entry_id":     entry.ID,
			}); err != nil {
				if !errors.Is(err, errAttachmentClaimConflict) {
					return fmt.Errorf("附件挂载足迹失败 entry=%d footprint=%d: %w", entry.ID, fp.ID, err)
				}
				log.Printf("[Footprint Sync] 附件归属冲突，已跳过 entry=%d footprint=%d: %v", entry.ID, fp.ID, err)
			}
		} else {
			var entryAttIDs []uint
			targetDB.Model(&models.Attachment{}).
				Where("entry_id = ? AND (footprint_id IS NULL OR footprint_id = 0)", entry.ID).
				Pluck("id", &entryAttIDs)
			if err := claimAttachments(targetDB, entryAttIDs, map[string]any{
				"footprint_id": fp.ID,
				"entry_id":     entry.ID,
			}); err != nil {
				if !errors.Is(err, errAttachmentClaimConflict) {
					return fmt.Errorf("附件挂载足迹失败 entry=%d footprint=%d: %w", entry.ID, fp.ID, err)
				}
				log.Printf("[Footprint Sync] 附件归属冲突，已跳过 entry=%d footprint=%d: %v", entry.ID, fp.ID, err)
			}
		}
	}
	return nil
}

var syncFootprintLock sync.Mutex

// footprintBackfillBatchSize 启动回填每批处理条数：兼顾吞吐与 SQLite 单次写锁持有时间。
const footprintBackfillBatchSize = 500

// EnsureEntryFootprintsSynced 确保所有带 city_code 的一级记录均已自然下渗并按同城连续日期归并为旅程。
func EnsureEntryFootprintsSynced() {
	if db.DB == nil {
		return
	}
	start := time.Now()
	syncFootprintLock.Lock()
	defer syncFootprintLock.Unlock()

	scannedEntries := 0
	bindingFixes := 0
	var lastEntryID uint
	for {
		var batch []models.LifeEntry
		if err := db.DB.
			Where("city_code != '' AND city_code IS NOT NULL AND type != 'wish'").
			Where("(footprint_id IS NULL OR footprint_id = 0) AND id NOT IN (SELECT life_entry_id FROM footprints WHERE life_entry_id IS NOT NULL AND deleted_at IS NULL)").
			Where("id > ?", lastEntryID).
			Order("id ASC").
			Limit(footprintBackfillBatchSize).
			Find(&batch).Error; err != nil {
			log.Printf("[Footprint Backfill] 散页→足迹查询失败（已处理 %d 条）: %v", scannedEntries, err)
			return
		}
		if len(batch) == 0 {
			break
		}
		batchLastID := batch[len(batch)-1].ID
		if err := db.DB.Transaction(func(tx *gorm.DB) error {
			for i := range batch {
				if err := syncFootprintFromLifeEntry(&batch[i], nil, tx); err != nil {
					log.Printf("[Footprint Backfill] 下渗失败 entry=%d: %v", batch[i].ID, err)
				}
			}
			return nil
		}); err != nil {
			log.Printf("[Footprint Backfill] 散页→足迹批次写入失败（batch last id=%d）: %v", batchLastID, err)
			return
		}
		scannedEntries += len(batch)
		lastEntryID = batchLastID
	}

	// 将已有 LifeEntryID 绑定但 FootprintID 为空的记录补齐 FootprintID。
	var lastFootprintID uint
	for {
		var unlinkedFps []models.Footprint
		if err := db.DB.Select("id, life_entry_id").
			Where("life_entry_id IS NOT NULL AND life_entry_id > 0").
			Where("life_entry_id IN (SELECT id FROM life_entries WHERE (footprint_id IS NULL OR footprint_id = 0) AND deleted_at IS NULL)").
			Where("id > ?", lastFootprintID).
			Order("id ASC").
			Limit(footprintBackfillBatchSize).
			Find(&unlinkedFps).Error; err != nil {
			log.Printf("[Footprint Backfill] 足迹反向绑定查询失败: %v", err)
			return
		}
		if len(unlinkedFps) == 0 {
			break
		}
		batchLastID := unlinkedFps[len(unlinkedFps)-1].ID
		if err := db.DB.Transaction(func(tx *gorm.DB) error {
			for _, fp := range unlinkedFps {
				if fp.LifeEntryID == nil || *fp.LifeEntryID == 0 {
					continue
				}
				res := tx.Model(&models.LifeEntry{}).
					Where("id = ? AND (footprint_id IS NULL OR footprint_id = 0)", *fp.LifeEntryID).
					Update("footprint_id", fp.ID)
				if res.Error != nil {
					return res.Error
				}
				bindingFixes += int(res.RowsAffected)
			}
			return nil
		}); err != nil {
			log.Printf("[Footprint Backfill] 足迹反向绑定批次写入失败（batch last id=%d）: %v", batchLastID, err)
			return
		}
		lastFootprintID = batchLastID
	}

	log.Printf("[Footprint Backfill] 散页→足迹回填完成：扫描 %d 条，补齐反向绑定 %d 条，耗时 %s",
		scannedEntries, bindingFixes, time.Since(start).Round(time.Millisecond))
}

var footprintBackfillOnce sync.Once

// StartFootprintBackfill 在 HTTP server 启动后异步执行启动回填。
func StartFootprintBackfill() {
	footprintBackfillOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Footprint Backfill] 启动回填异常崩溃: %v", r)
				}
			}()

			// 后台回填跨越多次查询与事务，必须与整库恢复互斥
			db.LockRead()
			defer db.UnlockRead()

			start := time.Now()
			log.Println("[Footprint Backfill] 启动回填开始（后台执行，不阻塞 HTTP 监听）")
			EnsureEntryFootprintsSynced()
			EnsureFootprintTransactionsLinked()
			log.Printf("[Footprint Backfill] 启动回填结束，总耗时 %s", time.Since(start).Round(time.Millisecond))
		}()
	})
}
