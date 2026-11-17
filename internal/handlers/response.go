package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"loose-leaf/internal/config"

	"github.com/gin-gonic/gin"
)

// Response 统一标准响应结构
type Response struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code:    200,
		Message: "success",
		Data:    data,
	})
}

func Error(c *gin.Context, status int, message string) {
	log.Printf("[Handler Error %d] %s", status, message)
	c.JSON(status, Response{
		Code:    status,
		Message: message,
		Data:    nil,
	})
}

// setAuthVaryHeader 统一写出角色敏感响应的 Vary 头，防止共享缓存混用访客与管理员响应。
func setAuthVaryHeader(c *gin.Context) {
	c.Header("Vary", "Authorization, X-Device-Token, Cookie")
}

// ParseFlexTime 弹性解析前端传入的各种时间格式 (含 HTML5 datetime-local 标准格式 YYYY-MM-DDTHH:mm)
// 时区契约 (RC-134)：无时区后缀的串一律按中国北京时间解释；带偏移/RFC3339 串按自身偏移解析。
// 返回值统一携带 +08:00 偏移，JSON 序列化为 "2026-10-09T14:20:00+08:00"，前端按北京时间展示零漂移。
func ParseFlexTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return config.NowInBeijing(), nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006.01.02 15:04",
		"2006.01.02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, config.BeijingLocation); err == nil {
			return t.In(config.BeijingLocation), nil
		}
	}
	return config.NowInBeijing(), fmt.Errorf("unable to parse time string: %s", s)
}

// ParseSyncSinceTime 解析增量同步用于比较 updated_at / deleted_at 的时间过滤参数。
// 与数据库审计时间戳比较时统一转换为服务器本地时区 Local()，确保任意部署时区下比对无偏差。
func ParseSyncSinceTime(s string) (time.Time, error) {
	t, err := ParseFlexTime(s)
	if err != nil {
		return t, err
	}
	return t.Local(), nil
}

// parseIDParam 校验路由 :id 为正整数并返回（发现 5）。
func parseIDParam(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		Error(c, http.StatusBadRequest, "资源 ID 必须为正整数")
		return 0, false
	}
	return id, true
}

func hasJSONValue(fields map[string]json.RawMessage, key string) bool {
	value, exists := fields[key]
	return exists && strings.TrimSpace(string(value)) != "null"
}
