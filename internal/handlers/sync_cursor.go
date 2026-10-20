package handlers

import (
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 增量同步游标的统一契约 (MOB-P1-36)
//
// 背景：Android 与 macOS 两端都用「时间戳游标 + ?since=」做增量拉取，
// Web 端走 ETag/304 全量协商（从不发 since），因此本文件的改动**只影响 Android 与 macOS**，
// Web 端行为完全不变。
//
// 三处改动，全部向后兼容：
//
//  1. 边界由严格大于改为**含等号**（>=）。
//     原先用 `updated_at > ?`，而客户端把游标推进到「本次收到的最大 updated_at」，
//     于是**恰好等于游标那一秒**内发生的修改会被下一轮漏掉（漏拉是静默的，最难发现）。
//     改为含等号后，那条记录会在每一轮增量中被重复带回 —— 但三端都是按服务端 id upsert，
//     重复写入幂等，不会产生脏数据；而漏拉被彻底消除。
//
//  2. 响应头 `X-Sync-Cursor` 携带**服务端权威游标**（本次响应内记录的最大变动时间，RFC3339 北京时间）。
//     客户端可直接把它原样塞回 `?since=`，不必再在本地遍历全部记录求最大值
//     （Android 端原先要做上万个时间戳解析，是下拉刷新卡顿的主因之一）。
//     老客户端不认识这个响应头，行为完全不变。
//
//  3. 所有带 `Vary` 的响应补上 `Cookie` —— 一旦引入 Cookie 鉴权，
//     缺了它会让 CDN/反代把「带 Cookie」与「不带 Cookie」的响应混用同一份缓存。
//     这属于**现在就存在的隐患**，先补上以免将来启用 Cookie 时踩坑。
//
// 游标安全边界（重要）：
// 响应头里的游标是**本次响应这一页**的最大变动时间，不是全库最大值。
// 客户端必须对多页结果取最大值后再作为下一次的 since ——
// 若直接采用「全库最大值」，就会跳过「本次没拉到、但确实已更新」的记录，造成静默漏拉。

// SyncCursorHeader 服务端权威增量游标的响应头名
const SyncCursorHeader = "X-Sync-Cursor"

// applySyncSince 施加增量游标过滤（含等号，详见文件头说明）
func applySyncSince(query *gorm.DB, sinceStr string) *gorm.DB {
	if sinceStr == "" {
		return query
	}
	if sinceTime, err := ParseSyncSinceTime(sinceStr); err == nil {
		// MOB-P1-36：>= 而非 >，避免「恰好等于游标那一秒」的修改被漏拉
		return query.Where("updated_at >= ? OR deleted_at >= ?", sinceTime, sinceTime)
	}
	return query
}

// setSyncCursor 写出服务端权威增量游标
func setSyncCursor(c *gin.Context, t time.Time) {
	if t.IsZero() {
		return
	}
	c.Header(SyncCursorHeader, t.In(config.BeijingLocation).Format(time.RFC3339))
}

// entriesSyncCursor 计算生活档案响应中的「最后变动时间」最大值（含软删除时间）。
//
// 与 ETag 用的 maxUpdated 分开算：不改动既有 ETag 语义（BE-05），
// 只为增量游标提供更准确的上界 —— 软删除的时间戳必须计入，
// 否则客户端的游标会永远停在删除之前，墓碑记录每轮增量都被重复拉回。
func entriesSyncCursor(entries []models.LifeEntry) time.Time {
	var max time.Time
	for _, e := range entries {
		if e.UpdatedAt.After(max) {
			max = e.UpdatedAt
		}
		if e.DeletedAt.Valid && e.DeletedAt.Time.After(max) {
			max = e.DeletedAt.Time
		}
	}
	return max
}
