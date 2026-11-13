package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
)

var (
	publicModulesMu     sync.RWMutex
	publicModulesCached []string
	publicModulesLoaded bool
)

// InvalidatePublicModulesCache 在站点配置变更或测试切换数据库时清空公开模块缓存。
func InvalidatePublicModulesCache() {
	publicModulesMu.Lock()
	publicModulesCached = nil
	publicModulesLoaded = false
	publicModulesMu.Unlock()
}

// AllowedPublicModules 定义允许主理人在展厅开放的模块白名单（contracts/contract.json）。
// 账本、资产、信贷渠道、分期计划、分类字典、设备管理、存储灾备、可见性配置永不开放。
var AllowedPublicModules = []string{
	"entries",
	"projects",
	"footprints",
	"wishes",
	"profile",
	"almanac",
}

// NormalizePublicModules 按白名单过滤并去重，保持规范顺序。
func NormalizePublicModules(input []string) []string {
	if len(input) == 0 {
		return []string{}
	}
	want := make(map[string]bool, len(input))
	for _, m := range input {
		clean := strings.ToLower(strings.TrimSpace(m))
		if clean != "" {
			want[clean] = true
		}
	}
	out := make([]string, 0, len(AllowedPublicModules))
	for _, allowed := range AllowedPublicModules {
		if want[allowed] {
			out = append(out, allowed)
		}
	}
	return out
}

// ParsePublicModulesRaw 从数据库 JSON 字符串解析并规范化公开模块列表。
func ParsePublicModulesRaw(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return []string{}
	}
	var parsed []string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return []string{}
	}
	return NormalizePublicModules(parsed)
}

// MarshalPublicModulesRaw 将公开模块列表序列化为数据库 JSON 字符串。
func MarshalPublicModulesRaw(modules []string) string {
	normalized := NormalizePublicModules(modules)
	b, err := json.Marshal(normalized)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// GetPublicModules 读取当前站点已开启公开展示的模块列表（默认空切片 = 全站私有）。
func GetPublicModules() []string {
	publicModulesMu.RLock()
	if publicModulesLoaded {
		out := make([]string, len(publicModulesCached))
		copy(out, publicModulesCached)
		publicModulesMu.RUnlock()
		return out
	}
	publicModulesMu.RUnlock()

	if db.DB == nil {
		return []string{}
	}
	var cfg models.SiteConfig
	if err := db.DB.Select("public_modules").Limit(1).Find(&cfg).Error; err != nil {
		return []string{}
	}
	mods := ParsePublicModulesRaw(cfg.PublicModulesRaw)

	publicModulesMu.Lock()
	publicModulesCached = make([]string, len(mods))
	copy(publicModulesCached, mods)
	publicModulesLoaded = true
	publicModulesMu.Unlock()

	return mods
}

// GetPublicModulesMap 返回公开模块集合，便于多模块联合接口（search / stats）快速判断。
func GetPublicModulesMap() map[string]bool {
	mods := GetPublicModules()
	m := make(map[string]bool, len(mods))
	for _, mod := range mods {
		m[mod] = true
	}
	return m
}

// IsModulePublic 判断指定模块当前是否对未登录访客开放。
func IsModulePublic(module string) bool {
	for _, mod := range GetPublicModules() {
		if mod == module {
			return true
		}
	}
	return false
}

// RequirePublicModuleOrAdmin 校验当前请求是否为已授权主理人，或目标模块已对访客开放。
// 未授权且模块未开放时直接返回 403 Forbidden 并中止。
func RequirePublicModuleOrAdmin(c *gin.Context, module string) bool {
	if middleware.IsAdmin(c) {
		return true
	}
	if IsModulePublic(module) {
		return true
	}
	Error(c, http.StatusForbidden, "该板块当前为私有状态，请先配对或登录后访问")
	return false
}

// PublicModuleETagSuffix 生成与角色及当前公开模块集合绑定的 ETag 后缀，
// 确保切换模块公开开关后访客缓存立即失效。
func PublicModuleETagSuffix(isAdmin bool) string {
	if isAdmin {
		return ":admin"
	}
	return ":visitor:" + strings.Join(GetPublicModules(), ",")
}
