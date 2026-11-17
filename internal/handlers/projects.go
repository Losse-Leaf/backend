package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// validProjectStatuses 项目状态白名单（四端统一中文四态）。
var validProjectStatuses = map[string]bool{
	"进行中": true,
	"已完成": true,
	"维护中": true,
	"已搁置": true,
}

type ProjectRequest struct {
	Title           string `json:"title" binding:"required"`
	Subtitle        string `json:"subtitle"`
	Description     string `json:"description"`
	Story           string `json:"story"`
	Category        string `json:"category"`
	GithubURL       string `json:"github_url"`
	Status          string `json:"status"`
	Order           int    `json:"order"`
	ClientRequestID string `json:"client_request_id"`
	IsPrivate       bool   `json:"is_private"`
}

func CreateProject(c *gin.Context) {
	var req ProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "项目标题必填且格式有效")
		return
	}

	clientRequestID := strings.TrimSpace(req.ClientRequestID)
	if existing, deleted, ok := findIdempotentReplay[models.Project](clientRequestID, func(p *models.Project) (uint, bool) {
		return p.ID, p.DeletedAt.Valid
	}); ok {
		replyIdempotentHit(c, existing, deleted, nil)
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		Error(c, http.StatusBadRequest, "项目标题必填")
		return
	}

	projectStatus := strings.TrimSpace(req.Status)
	if projectStatus == "" {
		projectStatus = "进行中"
	}
	if !validProjectStatuses[projectStatus] {
		Error(c, http.StatusBadRequest, "status 仅支持 进行中/已完成/维护中/已搁置")
		return
	}

	p := models.Project{
		Title:       title,
		Subtitle:    strings.TrimSpace(req.Subtitle),
		Description: strings.TrimSpace(req.Description),
		Story:       strings.TrimSpace(req.Story),
		Category:    strings.TrimSpace(req.Category),
		GithubURL:   strings.TrimSpace(req.GithubURL),
		Status:      projectStatus,
		Order:       req.Order,
		IsPrivate:   req.IsPrivate,
	}
	if clientRequestID != "" {
		p.ClientRequestID = &clientRequestID
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		_, err := createProjectCompanionEntry(tx, p)
		return err
	})
	if err != nil {
		if existing, deleted, ok := findIdempotentReplay[models.Project](clientRequestID, func(p *models.Project) (uint, bool) {
			return p.ID, p.DeletedAt.Valid
		}); ok {
			replyIdempotentHit(c, existing, deleted, nil)
			return
		}
		Error(c, http.StatusInternalServerError, "创建项目及伴生记录失败: "+err.Error())
		return
	}
	InvalidateStatsCache()

	Success(c, p)
}

// GetProjectsV1 获取项目列表 (支持增量游标与 ETag 缓存协商 BE-05)
func GetProjectsV1(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "projects") {
		return
	}
	isAdmin := middleware.IsAdmin(c)
	setAuthVaryHeader(c)

	sinceStr := c.Query("since")
	includeDeleted := c.Query("include_deleted") == "true" || sinceStr != ""
	if !validateSyncSince(c, sinceStr) || !allowDeletedScope(c, includeDeleted) {
		return
	}

	dbScope := db.DB
	if includeDeleted {
		dbScope = db.DB.Unscoped()
	}

	query := dbScope.Model(&models.Project{}).Order("`order` ASC, id DESC")
	if !isAdmin {
		query = query.Where("is_private = ?", false)
	}

	query = applySyncSince(query, &models.Project{}, sinceStr)

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "500"))
	if limit > 1000 {
		limit = 1000
	}
	if limit > 0 {
		query = query.Limit(limit)
	}
	offset, _ := strconv.Atoi(c.Query("offset"))
	if offset > 0 {
		query = query.Offset(offset)
	}

	agg, err := aggregateListQuery(query)
	if err != nil {
		Error(c, http.StatusInternalServerError, "获取项目列表失败")
		return
	}
	if sinceStr == "" && negotiateListCache(c, listETag(c, agg, PublicModuleETagSuffix(isAdmin))) {
		return
	}

	var projects []models.Project
	if err := query.Find(&projects).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取项目列表失败")
		return
	}

	if offset == 0 && sinceStr != "" {
		if err := injectTombstones(&projects, TombstoneEntityProject, sinceStr,
			func(p models.Project) uint { return p.ID },
			func(id uint, deletedAt time.Time) models.Project {
				return models.Project{
					ID:        id,
					CreatedAt: deletedAt,
					UpdatedAt: deletedAt,
					DeletedAt: gorm.DeletedAt{Time: deletedAt, Valid: true},
				}
			},
		); err != nil {
			Error(c, http.StatusInternalServerError, "获取项目列表失败")
			return
		}
	}

	setSyncCursor(c, computeSyncCursor(projects, func(p models.Project) (time.Time, gorm.DeletedAt) {
		return p.UpdatedAt, p.DeletedAt
	}))

	Success(c, projects)
}

func GetProjectByID(c *gin.Context) {
	if !RequirePublicModuleOrAdmin(c, "projects") {
		return
	}
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	var p models.Project
	if err := db.DB.First(&p, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "项目不存在")
		return
	}

	isAdmin := middleware.IsAdmin(c)
	if !isAdmin && p.IsPrivate {
		Error(c, http.StatusNotFound, "项目不存在")
		return
	}

	var entries []models.LifeEntry
	entryQuery := db.DB.Where("project_id = ?", p.ID).Order("occurred_at DESC")
	if !isAdmin {
		entryQuery = entryQuery.Where("is_private = ?", false)
	}
	entryQuery.Find(&entries)
	setAuthVaryHeader(c)
	populateEntryURLsBatch(entries)
	if !isAdmin {
		redactEmbeddedWishFinancesBatch(entries)
	}

	Success(c, gin.H{
		"project": p,
		"entries": entries,
	})
}

type ProjectUpdateRequest struct {
	Title       *string `json:"title"`
	Subtitle    *string `json:"subtitle"`
	Description *string `json:"description"`
	Story       *string `json:"story"`
	Category    *string `json:"category"`
	GithubURL   *string `json:"github_url"`
	Status      *string `json:"status"`
	Order       *int    `json:"order"`
	IsPrivate   *bool   `json:"is_private"`
}

func UpdateProject(c *gin.Context) {
	id := c.Param("id")
	var p models.Project
	if err := db.DB.First(&p, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "项目不存在")
		return
	}

	var req ProjectUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	oldStory := p.Story
	oldStatus := p.Status
	oldTitle := p.Title

	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" {
			Error(c, http.StatusBadRequest, "项目标题不能为空")
			return
		}
		p.Title = title
	}
	if req.Subtitle != nil {
		p.Subtitle = strings.TrimSpace(*req.Subtitle)
	}
	if req.Description != nil {
		p.Description = strings.TrimSpace(*req.Description)
	}
	if req.Story != nil {
		p.Story = strings.TrimSpace(*req.Story)
	}
	if req.Category != nil {
		p.Category = strings.TrimSpace(*req.Category)
	}
	if req.GithubURL != nil {
		p.GithubURL = strings.TrimSpace(*req.GithubURL)
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != "" {
			if !validProjectStatuses[status] {
				Error(c, http.StatusBadRequest, "status 仅支持 进行中/已完成/维护中/已搁置")
				return
			}
			p.Status = status
		}
	}
	if req.Order != nil {
		p.Order = *req.Order
	}
	if req.IsPrivate != nil {
		p.IsPrivate = *req.IsPrivate
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := syncProjectCompanionEntry(tx, p, oldTitle); err != nil {
			return err
		}

		if err := tx.Save(&p).Error; err != nil {
			return err
		}

		storyChanged := req.Story != nil && p.Story != "" && p.Story != oldStory
		statusChanged := req.Status != nil && p.Status != "" && p.Status != oldStatus
		if storyChanged || statusChanged {
			entryTitle := fmt.Sprintf("%s · 造物手记", p.Title)
			if statusChanged && !storyChanged {
				entryTitle = fmt.Sprintf("%s · 状态演进 (%s)", p.Title, p.Status)
			}
			entryContent := p.Story
			if !storyChanged || entryContent == "" {
				if p.Description != "" {
					entryContent = p.Description
				} else {
					entryContent = fmt.Sprintf("造物项目「%s」进展更新，当前状态：%s。", p.Title, p.Status)
				}
			}
			progressEntry := models.LifeEntry{
				Type:       "project",
				Title:      entryTitle,
				Content:    entryContent,
				OccurredAt: time.Now().In(config.BeijingLocation),
				ProjectID:  &p.ID,
			}
			if err := tx.Create(&progressEntry).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "更新项目及伴生进展失败: "+err.Error())
		return
	}
	InvalidateStatsCache()

	Success(c, p)
}

// DeleteProject 删除项目 (默认软删除，?purge=true 为物理清理 BE-05)
func DeleteProject(c *gin.Context) {
	id := c.Param("id")
	purge := c.Query("purge") == "true"

	var p models.Project
	if err := db.DB.First(&p, "id = ?", id).Error; err != nil {
		Error(c, http.StatusNotFound, "项目不存在或已被删除")
		return
	}

	if purge {
		var keysToDelete []string
		err := db.DB.Transaction(func(tx *gorm.DB) error {
			var companionIDs []uint
			if err := tx.Unscoped().Model(&models.LifeEntry{}).
				Where("project_id = ?", p.ID).
				Pluck("id", &companionIDs).Error; err != nil {
				return err
			}
			for _, entryID := range companionIDs {
				keys, err := releaseEntryAttachments(tx, entryID, true)
				if err != nil {
					return err
				}
				keysToDelete = append(keysToDelete, keys...)
			}
			if err := tx.Unscoped().Where("project_id = ?", p.ID).Delete(&models.LifeEntry{}).Error; err != nil {
				return err
			}
			if err := recordSyncTombstones(tx, TombstoneEntityEntry, companionIDs); err != nil {
				return err
			}
			if err := recordSyncTombstone(tx, TombstoneEntityProject, p.ID); err != nil {
				return err
			}
			return tx.Unscoped().Delete(&p).Error
		})
		if err != nil {
			Error(c, http.StatusInternalServerError, "物理删除失败: "+err.Error())
			return
		}
		for _, key := range keysToDelete {
			_ = storage.DeleteObject(key)
		}
		InvalidateStatsCache()
		Success(c, gin.H{"id": id, "purged": true})
		return
	}

	err := db.DB.Transaction(func(tx *gorm.DB) error {
		var companionIDs []uint
		if err := tx.Unscoped().Model(&models.LifeEntry{}).
			Where("project_id = ?", p.ID).
			Pluck("id", &companionIDs).Error; err != nil {
			return err
		}
		if err := deleteProjectCompanionEntries(tx, p.ID); err != nil {
			return err
		}
		for _, entryID := range companionIDs {
			if _, err := releaseEntryAttachments(tx, entryID, false); err != nil {
				return err
			}
		}
		return tx.Delete(&p).Error
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	InvalidateStatsCache()
	Success(c, gin.H{"id": id, "deleted": true})
}
