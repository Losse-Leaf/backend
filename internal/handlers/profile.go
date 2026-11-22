package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
)

func parseSocialMap(raw string) map[string]any {
	result := make(map[string]any)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return result
	}
	_ = json.Unmarshal([]byte(raw), &result)
	return result
}

func populateProfileData(p *models.CuratorProfile, isAdmin bool) {
	socialMap := parseSocialMap(p.SocialLinks)
	if emailVal, ok := socialMap["email"].(string); ok {
		p.Email = strings.TrimSpace(emailVal)
	}
	if githubVal, ok := socialMap["github"].(string); ok {
		p.Github = strings.TrimSpace(githubVal)
	}
	// 隐私分级：GitHub 为公开展示链接，邮箱属联系方式，仅管理员可见 (BE-39)
	if !isAdmin {
		p.Email = ""
		socialMap["email"] = ""
	}
	socialMap["github"] = p.Github
	p.SocialData = socialMap

	if p.AvatarURL != "" {
		p.AvatarKey = storage.ExtractObjectKey(p.AvatarURL)
		p.AvatarURL = storage.SignResourceURL(p.AvatarURL)
	}
}

// GetCuratorProfile 获取主理人基础档案 (全端动态获取)
func GetCuratorProfile(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "profile") {
		return
	}
	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		Error(c, http.StatusNotFound, "主理人档案尚未初始化")
		return
	}
	setAuthVaryHeader(c)
	isAdmin := middleware.IsAdmin(c)
	populateProfileData(&profile, isAdmin)
	Success(c, profile)
}

// UpdateCuratorProfileRequest 主理人更新载荷 (支持顶层 email/github 与 social_links 自动合并 RG-09)
type UpdateCuratorProfileRequest struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Bio         string `json:"bio"`
	Location    string `json:"location"`
	CurrentCity string `json:"current_city"`
	AvatarURL   string `json:"avatar_url"`
	AvatarKey   string `json:"avatar_key"`
	Motto       string `json:"motto"`
	Manifesto   string `json:"manifesto"`
	Email       string `json:"email"`
	Github      string `json:"github"`
	SocialLinks any    `json:"social_links"`
}

// UpdateCuratorProfile 更新主理人档案 (需管理员/已配对凭据)
func UpdateCuratorProfile(c *gin.Context) {
	var profile models.CuratorProfile
	db.DB.FirstOrCreate(&profile)

	rawBytes, err := c.GetRawData()
	if err != nil || len(rawBytes) == 0 {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	var req UpdateCuratorProfileRequest
	if err := json.Unmarshal(rawBytes, &req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	var rawKeys map[string]json.RawMessage
	_ = json.Unmarshal(rawBytes, &rawKeys)
	// 契约 C：与 handlers.go 的 hasJSONValue 同语义 —— 「携带 key 且值非 null」才写入，
	// 缺省 / null 一律保持原值。原先只判 key 存在，JSON null 会被当作「清空」处理，
	// 与「缺省/null = 保持原值」的跨端契约不符。
	hasKey := func(k string) bool {
		return hasJSONValue(rawKeys, k)
	}

	// 字段语义统一为 PATCH：请求携带该 key 即按其值写入（空串 = 清空），不再与 name 耦合 (BE-35)
	if req.Name != "" || hasKey("name") {
		profile.Name = strings.TrimSpace(req.Name)
	}
	if req.Title != "" || hasKey("title") {
		profile.Title = strings.TrimSpace(req.Title)
	}
	if req.Bio != "" || hasKey("bio") {
		profile.Bio = strings.TrimSpace(req.Bio)
	}
	if req.Location != "" || hasKey("location") {
		profile.Location = strings.TrimSpace(req.Location)
	}
	if req.CurrentCity != "" || hasKey("current_city") {
		profile.CurrentCity = strings.TrimSpace(req.CurrentCity)
	}

	avatarCandidate := ""
	if req.AvatarKey != "" {
		avatarCandidate = strings.TrimSpace(req.AvatarKey)
	} else if req.AvatarURL != "" {
		avatarCandidate = strings.TrimSpace(req.AvatarURL)
	}
	if avatarCandidate != "" {
		profile.AvatarURL = storage.ExtractObjectKey(avatarCandidate)
	}

	if req.Motto != "" || hasKey("motto") {
		profile.Motto = strings.TrimSpace(req.Motto)
	}
	if req.Manifesto != "" || hasKey("manifesto") {
		profile.Manifesto = strings.TrimSpace(req.Manifesto)
	}

	// 合并 social_links 与顶层 email / github 字段 (RG-09)
	if req.SocialLinks != nil || hasKey("email") || hasKey("github") {
		socialMap := parseSocialMap(profile.SocialLinks)
		if req.SocialLinks != nil {
			switch v := req.SocialLinks.(type) {
			case string:
				for k, val := range parseSocialMap(v) {
					socialMap[k] = val
				}
			case map[string]any:
				for k, val := range v {
					socialMap[k] = val
				}
			default:
				if bytes, err := json.Marshal(v); err == nil {
					for k, val := range parseSocialMap(string(bytes)) {
						socialMap[k] = val
					}
				}
			}
		}
		if req.Email != "" || hasKey("email") {
			socialMap["email"] = strings.TrimSpace(req.Email)
		}
		if req.Github != "" || hasKey("github") {
			socialMap["github"] = strings.TrimSpace(req.Github)
		}
		if bytes, err := json.Marshal(socialMap); err == nil {
			profile.SocialLinks = string(bytes)
		}
	}

	profile.UpdatedAt = time.Now()

	if err := db.DB.Save(&profile).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新主理人档案失败: "+err.Error())
		return
	}

	populateProfileData(&profile, true)
	Success(c, profile)
}
