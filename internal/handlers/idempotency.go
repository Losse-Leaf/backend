package handlers

import (
	"net/http"

	"loose-leaf/internal/db"

	"github.com/gin-gonic/gin"
)

// findIdempotentReplay 按 client_request_id 查询已有记录（含软删除行），用于写操作幂等去重。
func findIdempotentReplay[T any](clientRequestID string, inspect func(*T) (id uint, deleted bool)) (*T, bool, bool) {
	if clientRequestID == "" {
		return nil, false, false
	}
	var existing T
	if err := db.DB.Unscoped().Where("client_request_id = ?", clientRequestID).Limit(1).Find(&existing).Error; err != nil {
		return nil, false, false
	}
	id, deleted := inspect(&existing)
	if id == 0 {
		return nil, false, false
	}
	return &existing, deleted, true
}

// replyIdempotentHit 输出标准幂等重放响应：若原记录已软删除则返回 410 Gone，否则执行 prepare 并返回 200。
func replyIdempotentHit[T any](c *gin.Context, item *T, deleted bool, prepare func(*T)) {
	c.Header("X-Idempotent-Replay", "true")
	if deleted {
		Error(c, http.StatusGone, "记录已在服务端被删除")
		return
	}
	if prepare != nil {
		prepare(item)
	}
	Success(c, *item)
}
