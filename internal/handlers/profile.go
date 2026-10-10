package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"loose-leaf/internal/db"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
)

func parseJSONOrRaw(raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var obj any
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		return obj
	}
	return raw
}

func populateProfileData(p *models.CuratorProfile) {
	p.SocialData = parseJSONOrRaw(p.SocialLinks)
	p.EDCGearData = parseJSONOrRaw(p.EDCGear)
	if p.AvatarURL != "" {
		p.AvatarKey = storage.ExtractObjectKey(p.AvatarURL)
		p.AvatarURL = storage.SignResourceURL(p.AvatarURL)
	}
}

// GetCuratorProfile 获取主理人完整档案 (全端动态获取，禁止前端硬编码)
func GetCuratorProfile(c *gin.Context) {
	var profile models.CuratorProfile
	if err := db.DB.First(&profile).Error; err != nil {
		Error(c, http.StatusNotFound, "主理人档案尚未初始化")
		return
	}
	populateProfileData(&profile)
	Success(c, profile)
}

// UpdateCuratorProfileRequest 主理人更新载荷
type UpdateCuratorProfileRequest struct {
	Name        string `json:"name"`
	EnglishName string `json:"english_name"`
	Title       string `json:"title"`
	Bio         string `json:"bio"`
	Location    string `json:"location"`
	CurrentCity string `json:"current_city"`
	AvatarURL   string `json:"avatar_url"`
	AvatarKey   string `json:"avatar_key"`
	Avatar      string `json:"avatar"`
	Motto       string `json:"motto"`
	Manifesto   string `json:"manifesto"`
	SocialLinks any    `json:"social_links"`
	EDCGear     any    `json:"edc_gear"`
}

// UpdateCuratorProfile 更新主理人档案 (需管理员/已配对凭据)
func UpdateCuratorProfile(c *gin.Context) {
	var profile models.CuratorProfile
	db.DB.FirstOrCreate(&profile)

	var req UpdateCuratorProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	if req.Name != "" {
		profile.Name = strings.TrimSpace(req.Name)
	}
	if req.EnglishName != "" {
		profile.EnglishName = strings.TrimSpace(req.EnglishName)
	}
	if req.Title != "" {
		profile.Title = strings.TrimSpace(req.Title)
	}
	if req.Bio != "" {
		profile.Bio = strings.TrimSpace(req.Bio)
	}
	if req.Location != "" {
		profile.Location = strings.TrimSpace(req.Location)
	}
	if req.CurrentCity != "" {
		profile.CurrentCity = strings.TrimSpace(req.CurrentCity)
	}
	avatarCandidate := ""
	if req.AvatarKey != "" {
		avatarCandidate = strings.TrimSpace(req.AvatarKey)
	} else if req.AvatarURL != "" {
		avatarCandidate = strings.TrimSpace(req.AvatarURL)
	} else if req.Avatar != "" {
		avatarCandidate = strings.TrimSpace(req.Avatar)
	}
	if avatarCandidate != "" {
		profile.AvatarURL = storage.ExtractObjectKey(avatarCandidate)
	}
	if req.Motto != "" {
		profile.Motto = strings.TrimSpace(req.Motto)
	}
	if req.Manifesto != "" {
		profile.Manifesto = strings.TrimSpace(req.Manifesto)
	}

	if req.SocialLinks != nil {
		if str, ok := req.SocialLinks.(string); ok {
			profile.SocialLinks = str
		} else {
			bytes, _ := json.Marshal(req.SocialLinks)
			profile.SocialLinks = string(bytes)
		}
	}

	if req.EDCGear != nil {
		if str, ok := req.EDCGear.(string); ok {
			profile.EDCGear = str
		} else {
			bytes, _ := json.Marshal(req.EDCGear)
			profile.EDCGear = string(bytes)
		}
	}

	profile.UpdatedAt = time.Now()

	if err := db.DB.Save(&profile).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新主理人档案失败: "+err.Error())
		return
	}

	populateProfileData(&profile)
	Success(c, profile)
}
