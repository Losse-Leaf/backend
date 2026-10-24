package handlers

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"loose-leaf/internal/config"
)

// listAggregate 一次聚合查询的结果：
//   - Total：过滤后的总行数（用于分页 total 与 ETag，复用同一次 SQL）
//   - MaxUpdated / MaxDeleted：过滤后最新的 updated_at / deleted_at
//
// 关键（BE-2）：ETag 协商必须在分页 Find 与签名之前完成。
// 否则一个 304 也要先 COUNT + Find + 逐条签名，缓存协商反而比直接返回更贵。
type listAggregate struct {
	Total      int64
	MaxUpdated time.Time
	MaxDeleted time.Time
}

// aggregateListQuery 用一条聚合 SQL 同时算出 total / max(updated_at) / max(deleted_at)。
//
// 调用方传入的 query 已带上全部业务过滤（type/year/city/since...），
// 这里克隆一份并去掉排序与分页，避免把 LIMIT/OFFSET 带进聚合。
// 注意：非 Unscoped 查询里 deleted_at 恒为 NULL，MAX(deleted_at) 仍有列可算，
// 只是结果为空；Unscoped（增量同步 / include_deleted）时才会拿到墓碑时间。
func aggregateListQuery(query *gorm.DB) (listAggregate, error) {
	// SQLite 对 MAX(time_column) 这类聚合表达式不保留列声明类型，
	// driver 会回传字符串而不是 time.Time，因此先按 sql.NullString 接住再解析。
	var row struct {
		Total      int64
		MaxUpdated sql.NullString
		MaxDeleted sql.NullString
	}

	// Session(&gorm.Session{}) 克隆查询；先 Select 触发 statement 克隆，
	// 再从克隆体上摘掉 ORDER BY / LIMIT，避免影响原分页查询。
	aggQuery := query.Session(&gorm.Session{}).
		Select("COUNT(*) AS total, MAX(updated_at) AS max_updated, MAX(deleted_at) AS max_deleted")
	delete(aggQuery.Statement.Clauses, "ORDER BY")
	delete(aggQuery.Statement.Clauses, "LIMIT")

	if err := aggQuery.Scan(&row).Error; err != nil {
		return listAggregate{}, err
	}

	agg := listAggregate{Total: row.Total}
	if row.MaxUpdated.Valid {
		if t, err := parseAggregateTime(row.MaxUpdated.String); err == nil {
			agg.MaxUpdated = t
		}
	}
	if row.MaxDeleted.Valid {
		if t, err := parseAggregateTime(row.MaxDeleted.String); err == nil {
			agg.MaxDeleted = t
		}
	}
	return agg, nil
}

// parseAggregateTime 解析 SQLite 驱动返回的时间字符串。
// 兼容 GORM 写入的带纳秒/偏移格式，以及无偏移的本地时间格式。
func parseAggregateTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	layouts := []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	// 无偏移的历史格式按北京时间解释（RC-134）。带偏移的 layout 会以串内
	// 偏移为准，此处的 location 只对最后两种 naive 格式生效。
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, config.BeijingLocation); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间: %s", raw)
}

// lastModified 返回 updated_at / deleted_at 中较晚的一个，用于 If-Modified-Since。
func (a listAggregate) lastModified() time.Time {
	if a.MaxDeleted.After(a.MaxUpdated) {
		return a.MaxDeleted
	}
	return a.MaxUpdated
}

// listETag 生成与「完整查询串 + 权限角色」绑定的弱 ETag。
//
// 关键：必须把原始 RequestURI 的哈希纳入 ETag。
// 否则 /entries?page=1 与 /entries?page=2（或不同 year/filter）在 total 与
// max(updated_at) 恰好相同时会共用 ETag，客户端就会把另一页当成未修改。
// salt 用于心愿单等按角色脱敏的列表，防止跨权限 304 缓存污染（BE-17）。
func listETag(c *gin.Context, agg listAggregate, salt string) string {
	h := fnv.New32a()
	if c != nil && c.Request != nil && c.Request.URL != nil {
		_, _ = io.WriteString(h, c.Request.URL.RequestURI())
	}
	if salt != "" {
		_, _ = io.WriteString(h, "|"+salt)
	}
	// 列表响应里带的是**短期签名媒体地址**（本机 30 分钟 / R2 预签名）。
	// 若数据不变就永远 304，客户端会一直复用早已过期的 URL，相纸全部载入失败。
	// 把「25 分钟时间桶」混进 ETag：最多每 25 分钟强制换一次 body，拿到新签名。
	ttlBucket := time.Now().Unix() / int64(25*60)
	return fmt.Sprintf(`W/"%d-%d-%d-%d-%08x"`,
		agg.Total,
		agg.MaxUpdated.UnixMilli(),
		agg.MaxDeleted.UnixMilli(),
		ttlBucket,
		h.Sum32())
}

// negotiateListCache 写入 ETag 并在命中缓存时直接返回 304。
//
// 顺序固定：先 ETag，再 If-Modified-Since（兼容旧客户端）。
// 命中时调用方必须立刻 return —— 此时尚未 Find、尚未做任何 URL 签名。
func negotiateListCache(c *gin.Context, etag string, lastModified time.Time) bool {
	c.Header("ETag", etag)

	if match := c.GetHeader("If-None-Match"); match != "" && match == etag {
		c.Status(http.StatusNotModified)
		return true
	}

	// lastModified 为零值时跳过 IMS：按角色脱敏的列表（心愿 / 足迹）不能用
	// 时间戳做跨角色协商，否则访客缓存的响应可能在管理员登录后仍被判 304。
	if !lastModified.IsZero() {
		if ifModSince := c.GetHeader("If-Modified-Since"); ifModSince != "" {
			if t, err := http.ParseTime(ifModSince); err == nil && !lastModified.After(t) {
				c.Status(http.StatusNotModified)
				return true
			}
		}
	}
	return false
}
