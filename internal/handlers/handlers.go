package handlers

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"chimu-lab/internal/db"
	"chimu-lab/internal/models"
	"chimu-lab/internal/storage"

	"github.com/gin-gonic/gin"
)

var startTime = time.Now()

// 统一标准响应结构
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
	c.JSON(status, Response{
		Code:    status,
		Message: message,
		Data:    nil,
	})
}

// ParseFlexTime 弹性解析前端传入的各种时间格式 (含 HTML5 datetime-local 标准格式 YYYY-MM-DDTHH:mm)
func ParseFlexTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now(), nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006.01.02 15:04",
		"2006.01.02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Now(), fmt.Errorf("unable to parse time string: %s", s)
}

// formatDatesFromTime 根据 OccurredAt 自动格式化旧版兼容字段
func formatDatesFromTime(e *models.LifeEntry) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}
	e.Year = fmt.Sprintf("%04d", e.OccurredAt.Year())
	e.Month = fmt.Sprintf("%02d", e.OccurredAt.Month())
	e.Day = fmt.Sprintf("%02d", e.OccurredAt.Day())
	e.Time = fmt.Sprintf("%02d:%02d", e.OccurredAt.Hour(), e.OccurredAt.Minute())
	e.Date = fmt.Sprintf("%s.%s.%s", e.Year, e.Month, e.Day)
}

// populateAttachmentURLs 填充附件的动态临时预签名访问 URL (默认 30 分钟有效)
func populateAttachmentURLs(atts []models.Attachment) []models.Attachment {
	for i := range atts {
		atts[i].URL = storage.SignResourceURL(atts[i].ObjectKey)
	}
	return atts
}

// signImagesString 对逗号分割的图片相对路径或 URL 列表进行批量动态签名
func signImagesString(imagesStr string) string {
	imagesStr = strings.TrimSpace(imagesStr)
	if imagesStr == "" {
		return ""
	}
	parts := strings.Split(imagesStr, ",")
	var signedParts []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		signedParts = append(signedParts, storage.SignResourceURL(p))
	}
	return strings.Join(signedParts, ",")
}

// ==========================================
// 1. Life Entry CRUD (生活档案接口)
// ==========================================

// CreateEntryRequest 新增生活记录请求载荷
type CreateEntryRequest struct {
	Type          string `json:"type" binding:"required"` // life, thought, photo, project, collection
	Title         string `json:"title"`
	Content       string `json:"content" binding:"required"`
	OccurredAt    string `json:"occurred_at"` // ISO8601 或 "2006-01-02T15:04" 或 "2006-01-02 15:04" 或 "2006.01.02"
	Location      string `json:"location"`
	Tags          string `json:"tags"`
	Meta          string `json:"meta"`
	Link          string `json:"link"`
	Featured      bool   `json:"featured"`
	ProjectID     *uint  `json:"project_id"`
	AttachmentIDs []uint `json:"attachment_ids"`
}

// CreateLifeEntry 创建一条新的生活档案记录
func CreateLifeEntry(c *gin.Context) {
	var req CreateEntryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "内容必填，且必须指定有效类型")
		return
	}

	occurred, _ := ParseFlexTime(req.OccurredAt)

	entry := models.LifeEntry{
		Type:       req.Type,
		Title:      strings.TrimSpace(req.Title),
		Content:    strings.TrimSpace(req.Content),
		OccurredAt: occurred,
		Location:   strings.TrimSpace(req.Location),
		Tags:       strings.TrimSpace(req.Tags),
		Meta:       strings.TrimSpace(req.Meta),
		Link:       strings.TrimSpace(req.Link),
		Featured:   req.Featured,
		ProjectID:  req.ProjectID,
	}
	formatDatesFromTime(&entry)

	// 保存主体
	if err := db.DB.Create(&entry).Error; err != nil {
		Error(c, http.StatusInternalServerError, "保存记录失败: "+err.Error())
		return
	}

	// 关联已上传的附件
	if len(req.AttachmentIDs) > 0 {
		db.DB.Model(&models.Attachment{}).
			Where("id IN ?", req.AttachmentIDs).
			Update("entry_id", entry.ID)

		// 同步到旧版 images 字段以保证老组件兼容 (存相对路径 keys，查询时动态签名)
		var atts []models.Attachment
		db.DB.Where("id IN ?", req.AttachmentIDs).Find(&atts)
		if len(atts) > 0 {
			var keys []string
			for _, a := range atts {
				keys = append(keys, a.ObjectKey)
			}
			entry.Images = strings.Join(keys, ",")
			db.DB.Model(&entry).Update("images", entry.Images)
		}
	}

	// 重新预加载关联
	db.DB.Preload("Attachments").Preload("Project").First(&entry, entry.ID)
	entry.Attachments = populateAttachmentURLs(entry.Attachments)
	if entry.Images != "" {
		entry.Images = signImagesString(entry.Images)
	} else if len(entry.Attachments) > 0 {
		entry.Images = entry.Attachments[0].URL
	}

	Success(c, entry)
}

// GetLifeEntriesV1 列表查询生活档案 (支持分页与类型筛选)
func GetLifeEntriesV1(c *gin.Context) {
	entryType := c.Query("type")
	year := c.Query("year")
	category := c.Query("category") // 兼容 5 大核心视角 (daily, thought, project, collection, all)

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}

	query := db.DB.Model(&models.LifeEntry{}).
		Preload("Attachments").
		Preload("Project").
		Order("occurred_at DESC, id DESC")

	if entryType != "" && entryType != "all" {
		query = query.Where("type = ?", entryType)
	}
	if year != "" && year != "all" {
		query = query.Where("year = ?", year)
	}

	// 核心视角动态映射
	if category != "" && category != "all" {
		switch category {
		case "daily":
			query = query.Where("type IN ('daily', 'life', 'coffee', 'place', 'moment')")
		case "thought":
			query = query.Where("type IN ('thought', 'idea', 'note', 'essay')")
		case "project":
			query = query.Where("type IN ('project', 'code', 'craft')")
		case "collection":
			query = query.Where("type IN ('collection', 'music', 'book', 'game', 'purchase', 'gear', 'movie')")
		case "photo":
			query = query.Where("type = 'photo' OR images != ''")
		case "transaction":
			query = query.Where("type = 'transaction'")
		}
	}

	var total int64
	query.Count(&total)

	var entries []models.LifeEntry
	offset := (page - 1) * pageSize
	if err := query.Offset(offset).Limit(pageSize).Find(&entries).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取生活档案失败")
		return
	}

	for i := range entries {
		entries[i].Attachments = populateAttachmentURLs(entries[i].Attachments)
		// 动态签名现有 images 字段中的相对路径 (或回退到首个附件的动态签名地址)
		if entries[i].Images != "" {
			entries[i].Images = signImagesString(entries[i].Images)
		} else if len(entries[i].Attachments) > 0 {
			entries[i].Images = entries[i].Attachments[0].URL
		}
	}

	Success(c, gin.H{
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"entries":   entries,
	})
}

// GetLifeEntryByID 获取单条生活档案详情
func GetLifeEntryByID(c *gin.Context) {
	id := c.Param("id")
	var entry models.LifeEntry
	if err := db.DB.Preload("Attachments").Preload("Project").First(&entry, id).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该条生活记录")
		return
	}

	entry.Attachments = populateAttachmentURLs(entry.Attachments)
	if entry.Images != "" {
		entry.Images = signImagesString(entry.Images)
	} else if len(entry.Attachments) > 0 {
		entry.Images = entry.Attachments[0].URL
	}

	Success(c, entry)
}

// UpdateEntryRequest 更新生活记录请求载荷（字段皆可选）
type UpdateEntryRequest struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	OccurredAt    string `json:"occurred_at"`
	Location      string `json:"location"`
	Tags          string `json:"tags"`
	Meta          string `json:"meta"`
	Link          string `json:"link"`
	Featured      *bool  `json:"featured"`
	ProjectID     *uint  `json:"project_id"`
	AttachmentIDs []uint `json:"attachment_ids"`
}

// UpdateLifeEntry 更新生活记录
func UpdateLifeEntry(c *gin.Context) {
	id := c.Param("id")
	var entry models.LifeEntry
	if err := db.DB.Preload("Attachments").First(&entry, id).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该条生活记录")
		return
	}

	var req UpdateEntryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	if req.Type != "" {
		entry.Type = req.Type
	}
	if req.Title != "" {
		entry.Title = strings.TrimSpace(req.Title)
	}
	if req.Content != "" {
		entry.Content = strings.TrimSpace(req.Content)
	}
	if req.Location != "" {
		entry.Location = strings.TrimSpace(req.Location)
	}
	if req.Tags != "" {
		entry.Tags = strings.TrimSpace(req.Tags)
	}
	if req.Meta != "" {
		entry.Meta = strings.TrimSpace(req.Meta)
	}
	if req.Link != "" {
		entry.Link = strings.TrimSpace(req.Link)
	}
	if req.Featured != nil {
		entry.Featured = *req.Featured
	}
	entry.ProjectID = req.ProjectID

	if req.OccurredAt != "" {
		if t, err := ParseFlexTime(req.OccurredAt); err == nil {
			entry.OccurredAt = t
		}
		formatDatesFromTime(&entry)
	}

	if err := db.DB.Save(&entry).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新记录失败")
		return
	}

	// 更新关联附件
	if req.AttachmentIDs != nil {
		// 解除未包含的附件
		db.DB.Model(&models.Attachment{}).
			Where("entry_id = ? AND id NOT IN ?", entry.ID, req.AttachmentIDs).
			Update("entry_id", nil)

		// 绑定新选中的附件
		if len(req.AttachmentIDs) > 0 {
			db.DB.Model(&models.Attachment{}).
				Where("id IN ?", req.AttachmentIDs).
				Update("entry_id", entry.ID)
		}
	}

	db.DB.Preload("Attachments").Preload("Project").First(&entry, entry.ID)
	entry.Attachments = populateAttachmentURLs(entry.Attachments)
	if entry.Images != "" {
		entry.Images = signImagesString(entry.Images)
	} else if len(entry.Attachments) > 0 {
		entry.Images = entry.Attachments[0].URL
	}

	Success(c, entry)
}

// DeleteLifeEntry 删除生活记录及关联的附件与 R2 对象
func DeleteLifeEntry(c *gin.Context) {
	id := c.Param("id")
	var entry models.LifeEntry
	if err := db.DB.Preload("Attachments").First(&entry, id).Error; err != nil {
		Error(c, http.StatusNotFound, "记录不存在或已被删除")
		return
	}

	// 级联清理 R2 存储中的对象文件
	for _, att := range entry.Attachments {
		if att.ObjectKey != "" {
			_ = storage.DeleteObject(att.ObjectKey)
		}
	}

	// 删除数据库记录 (GORM 会自动外键级联删除 attachments)
	if err := db.DB.Delete(&entry).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}

	Success(c, gin.H{"id": id, "deleted": true})
}

// ==========================================
// 2. Uploads & Storage (R2 媒体上传接口)
// ==========================================

type PresignRequest struct {
	Filename    string `json:"filename" binding:"required"`
	ContentType string `json:"content_type" binding:"required"`
	Size        int64  `json:"size" binding:"required"`
}

// PresignUpload 生成 Presigned PUT URL
func PresignUpload(c *gin.Context) {
	var req PresignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少必要的文件信息 (filename, content_type, size)")
		return
	}

	// 大小限制：单文件最大 20MB
	maxSize := int64(20 * 1024 * 1024)
	if req.Size > maxSize {
		Error(c, http.StatusBadRequest, "文件过大，单张图片不能超过 20MB")
		return
	}

	// MIME 类型基本检查
	ct := strings.ToLower(req.ContentType)
	if !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "video/") && ct != "application/pdf" {
		Error(c, http.StatusBadRequest, "目前仅支持上传图片或媒体文件")
		return
	}

	uploadID, objectKey, presignedURL, expiresIn, err := storage.GeneratePresignedPutURL(req.Filename, req.ContentType, req.Size)
	if err != nil {
		Error(c, http.StatusServiceUnavailable, err.Error())
		return
	}

	Success(c, gin.H{
		"upload_id":   uploadID,
		"object_key":  objectKey,
		"upload_url":  presignedURL,
		"expires_in":  expiresIn,
	})
}

type CompleteUploadRequest struct {
	UploadID         string `json:"upload_id"`
	ObjectKey        string `json:"object_key" binding:"required"`
	OriginalFilename string `json:"original_filename"`
	MimeType         string `json:"mime_type"`
	Size             int64  `json:"size"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
}

// CompleteUpload 确认上传完成并创建 Attachment 实体
func CompleteUpload(c *gin.Context) {
	var req CompleteUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少 object_key")
		return
	}

	// 如果 R2 已配置，验证对象确实存在于存储桶中
	if storage.IsConfigured() {
		realSize, realContentType, err := storage.VerifyObjectExists(req.ObjectKey)
		if err != nil {
			Error(c, http.StatusBadRequest, "存储端未检测到上传的文件，请重试")
			return
		}
		if realSize > 0 {
			req.Size = realSize
		}
		if realContentType != "" {
			req.MimeType = realContentType
		}
	}

	att := models.Attachment{
		ObjectKey:        req.ObjectKey,
		OriginalFilename: req.OriginalFilename,
		MimeType:         req.MimeType,
		Size:             req.Size,
		Width:            req.Width,
		Height:           req.Height,
	}

	if err := db.DB.Create(&att).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建附件记录失败")
		return
	}

	att.URL = storage.SignResourceURL(att.ObjectKey)
	Success(c, att)
}

type CleanupUploadRequest struct {
	ObjectKeys []string `json:"object_keys" binding:"required"`
}

// CleanupUpload 清理未保存引用的孤立上传对象
func CleanupUpload(c *gin.Context) {
	var req CleanupUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少 object_keys")
		return
	}

	cleaned := 0
	for _, key := range req.ObjectKeys {
		if key != "" {
			var att models.Attachment
			// 仅允许清理在数据库中且未关联生活条目的孤立附件，防止越权传入他人 ObjectKey 随意删除
			if err := db.DB.Where("object_key = ? AND entry_id IS NULL", key).First(&att).Error; err == nil {
				_ = storage.DeleteObject(key)
				db.DB.Delete(&att)
				cleaned++
			}
		}
	}

	Success(c, gin.H{"cleaned_count": cleaned})
}

// GetStorageStatus 获取 R2 配置状态
func GetStorageStatus(c *gin.Context) {
	Success(c, storage.GetStorageStatus())
}

// ==========================================
// 3. Transactions CRUD (记账接口)
// ==========================================

type TransactionRequest struct {
	Amount        int64  `json:"amount" binding:"required"` // 分
	Type          string `json:"type" binding:"required"`   // income, expense
	Category      string `json:"category" binding:"required"`
	Title         string `json:"title"`
	Note          string `json:"note"`
	PaymentMethod string `json:"payment_method"`
	OccurredAt    string `json:"occurred_at"`
}

// CreateTransaction 记一笔
func CreateTransaction(c *gin.Context) {
	var req TransactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请填写金额、类型(支出/收入)与分类")
		return
	}

	occurred, _ := ParseFlexTime(req.OccurredAt)

	tx := models.Transaction{
		Amount:        req.Amount,
		Type:          req.Type,
		Category:      strings.TrimSpace(req.Category),
		Title:         strings.TrimSpace(req.Title),
		Note:          strings.TrimSpace(req.Note),
		PaymentMethod: strings.TrimSpace(req.PaymentMethod),
		OccurredAt:    occurred,
	}

	if err := db.DB.Create(&tx).Error; err != nil {
		Error(c, http.StatusInternalServerError, "记账失败: "+err.Error())
		return
	}

	Success(c, tx)
}

// GetTransactions 列表查询账目 (支持按年、按月)
func GetTransactions(c *gin.Context) {
	yearStr := c.Query("year")
	monthStr := c.Query("month")

	query := db.DB.Order("occurred_at DESC, id DESC")

	if yearStr != "" {
		y, err := strconv.Atoi(yearStr)
		if err == nil {
			if monthStr != "" {
				m, err := strconv.Atoi(monthStr)
				if err == nil {
					start := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.Local)
					end := start.AddDate(0, 1, 0)
					query = query.Where("occurred_at >= ? AND occurred_at < ?", start, end)
				}
			} else {
				start := time.Date(y, 1, 1, 0, 0, 0, 0, time.Local)
				end := start.AddDate(1, 0, 0)
				query = query.Where("occurred_at >= ? AND occurred_at < ?", start, end)
			}
		}
	}

	var txs []models.Transaction
	if err := query.Find(&txs).Error; err != nil {
		Error(c, http.StatusInternalServerError, "获取记账列表失败")
		return
	}

	Success(c, txs)
}

// GetTransactionSummary 月度记账统计
func GetTransactionSummary(c *gin.Context) {
	yearStr := c.DefaultQuery("year", fmt.Sprintf("%04d", time.Now().Year()))
	monthStr := c.DefaultQuery("month", fmt.Sprintf("%02d", time.Now().Month()))

	y, _ := strconv.Atoi(yearStr)
	m, _ := strconv.Atoi(monthStr)

	start := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 1, 0)

	var txs []models.Transaction
	db.DB.Where("occurred_at >= ? AND occurred_at < ?", start, end).Find(&txs)

	var totalExpense int64
	var totalIncome int64
	categoryBreakdown := make(map[string]int64)

	for _, tx := range txs {
		if tx.Type == "expense" {
			totalExpense += tx.Amount
			categoryBreakdown[tx.Category] += tx.Amount
		} else if tx.Type == "income" {
			totalIncome += tx.Amount
		}
	}

	Success(c, gin.H{
		"year":               y,
		"month":              m,
		"total_expense":      totalExpense,
		"total_income":       totalIncome,
		"balance":            totalIncome - totalExpense,
		"category_breakdown": categoryBreakdown,
		"count":              len(txs),
	})
}

// UpdateTransaction 更新账目
func UpdateTransaction(c *gin.Context) {
	id := c.Param("id")
	var tx models.Transaction
	if err := db.DB.First(&tx, id).Error; err != nil {
		Error(c, http.StatusNotFound, "账目记录不存在")
		return
	}

	var req TransactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	tx.Amount = req.Amount
	tx.Type = req.Type
	tx.Category = strings.TrimSpace(req.Category)
	tx.Title = strings.TrimSpace(req.Title)
	tx.Note = strings.TrimSpace(req.Note)
	tx.PaymentMethod = strings.TrimSpace(req.PaymentMethod)

	if req.OccurredAt != "" {
		if t, err := ParseFlexTime(req.OccurredAt); err == nil {
			tx.OccurredAt = t
		}
	}

	if err := db.DB.Save(&tx).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新账目失败")
		return
	}

	Success(c, tx)
}

// DeleteTransaction 删除账目
func DeleteTransaction(c *gin.Context) {
	id := c.Param("id")
	if err := db.DB.Delete(&models.Transaction{}, id).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除失败")
		return
	}
	Success(c, gin.H{"id": id, "deleted": true})
}

// ==========================================
// 4. Projects CRUD (项目与造物接口)
// ==========================================

type ProjectRequest struct {
	Title       string `json:"title"`
	Name        string `json:"name"` // 兼容入参
	Subtitle    string `json:"subtitle"`
	Description string `json:"description"`
	Story       string `json:"story"`
	Content     string `json:"content"` // 兼容入参
	Category    string `json:"category"`
	Tags        string `json:"tags"`
	GithubURL   string `json:"github_url"`
	Status      string `json:"status"`
	Featured    bool   `json:"featured"`
	Order       int    `json:"order"`
}

func CreateProject(c *gin.Context) {
	var req ProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(req.Name)
	}
	if title == "" {
		Error(c, http.StatusBadRequest, "项目标题必填")
		return
	}

	story := strings.TrimSpace(req.Story)
	if story == "" {
		story = strings.TrimSpace(req.Content)
	}

	p := models.Project{
		Title:       title,
		Subtitle:    strings.TrimSpace(req.Subtitle),
		Description: strings.TrimSpace(req.Description),
		Story:       story,
		Category:    strings.TrimSpace(req.Category),
		Tags:        strings.TrimSpace(req.Tags),
		GithubURL:   strings.TrimSpace(req.GithubURL),
		Status:      strings.TrimSpace(req.Status),
		Featured:    req.Featured,
		Order:       req.Order,
	}
	if p.Status == "" {
		p.Status = "Active"
	}

	if err := db.DB.Create(&p).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建项目失败")
		return
	}

	Success(c, p)
}

func GetProjectsV1(c *gin.Context) {
	var projects []models.Project
	db.DB.Order("`order` ASC, id DESC").Find(&projects)
	Success(c, projects)
}

func GetProjectByID(c *gin.Context) {
	id := c.Param("id")
	var p models.Project
	if err := db.DB.First(&p, id).Error; err != nil {
		Error(c, http.StatusNotFound, "项目不存在")
		return
	}

	// 同时拉取关联该项目的 Life Entries
	var entries []models.LifeEntry
	db.DB.Where("project_id = ?", p.ID).Preload("Attachments").Order("occurred_at DESC").Find(&entries)
	for i := range entries {
		entries[i].Attachments = populateAttachmentURLs(entries[i].Attachments)
	}

	Success(c, gin.H{
		"project": p,
		"entries": entries,
	})
}

func UpdateProject(c *gin.Context) {
	id := c.Param("id")
	var p models.Project
	if err := db.DB.First(&p, id).Error; err != nil {
		Error(c, http.StatusNotFound, "项目不存在")
		return
	}

	var req ProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(req.Name)
	}
	if title != "" {
		p.Title = title
	}
	if req.Subtitle != "" {
		p.Subtitle = strings.TrimSpace(req.Subtitle)
	}
	if req.Description != "" {
		p.Description = strings.TrimSpace(req.Description)
	}
	story := strings.TrimSpace(req.Story)
	if story == "" {
		story = strings.TrimSpace(req.Content)
	}
	if story != "" {
		p.Story = story
	}
	if req.Category != "" {
		p.Category = strings.TrimSpace(req.Category)
	}
	if req.Tags != "" {
		p.Tags = strings.TrimSpace(req.Tags)
	}
	if req.GithubURL != "" {
		p.GithubURL = strings.TrimSpace(req.GithubURL)
	}
	if req.Status != "" {
		p.Status = strings.TrimSpace(req.Status)
	}
	p.Featured = req.Featured
	p.Order = req.Order

	db.DB.Save(&p)
	Success(c, p)
}

func DeleteProject(c *gin.Context) {
	id := c.Param("id")
	db.DB.Delete(&models.Project{}, id)
	Success(c, gin.H{"id": id, "deleted": true})
}

// ==========================================
// 5. Global Stats (生活档案统计概览)
// ==========================================

func GetGlobalStats(c *gin.Context) {
	var totalEntries int64
	db.DB.Model(&models.LifeEntry{}).Count(&totalEntries)

	var totalPhotos int64
	db.DB.Model(&models.Attachment{}).Count(&totalPhotos)
	if totalPhotos == 0 {
		db.DB.Model(&models.LifeEntry{}).Where("type = 'photo' OR images != ''").Count(&totalPhotos)
	}

	var totalProjects int64
	db.DB.Model(&models.Project{}).Count(&totalProjects)

	// 当月支出
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 1, 0)

	var monthExpense int64
	type Result struct {
		Total int64
	}
	var res Result
	db.DB.Model(&models.Transaction{}).
		Select("COALESCE(SUM(amount), 0) as total").
		Where("type = 'expense' AND occurred_at >= ? AND occurred_at < ?", start, end).
		Scan(&res)
	monthExpense = res.Total

	Success(c, gin.H{
		"total_entries": totalEntries,
		"total_photos":  totalPhotos,
		"total_projects": totalProjects,
		"month_expense": monthExpense,
		"current_year":  now.Year(),
		"current_month": int(now.Month()),
	})
}

// ==========================================
// 6. Legacy Compatible Routes (平滑兼容原有旧路由)


func GetConfigV1(c *gin.Context) {
	var config models.SiteConfig
	if err := db.DB.First(&config).Error; err != nil {
		config = models.SiteConfig{
			SiteName:        "散页 · 时光手账与生活档案",
			SiteSubtitle:    "时光手账与生活档案 · 记录真实可感的日子",
			CuratorName:     "迟暮",
			CuratorEn:       "",
			DefaultLocation: "中国 · 杭州",
			Domain:          "codeactivityhub.top",
			ICPNumber:       "浙ICP备2026081664号",
			ICPLink:         "https://beian.miit.gov.cn",
		}
	}
	Success(c, config)
}

func UpdateConfig(c *gin.Context) {
	var config models.SiteConfig
	db.DB.FirstOrCreate(&config)

	var req models.SiteConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效")
		return
	}

	if req.SiteName != "" {
		config.SiteName = strings.TrimSpace(req.SiteName)
	}
	if req.SiteSubtitle != "" {
		config.SiteSubtitle = strings.TrimSpace(req.SiteSubtitle)
	}
	if req.CuratorName != "" {
		config.CuratorName = strings.TrimSpace(req.CuratorName)
	}
	if req.CuratorEn != "" {
		config.CuratorEn = strings.TrimSpace(req.CuratorEn)
	}
	if req.DefaultLocation != "" {
		config.DefaultLocation = strings.TrimSpace(req.DefaultLocation)
	}
	if req.SiteDesc != "" {
		config.SiteDesc = strings.TrimSpace(req.SiteDesc)
	}
	if req.Domain != "" {
		config.Domain = strings.TrimSpace(req.Domain)
	}
	if req.ICPNumber != "" {
		config.ICPNumber = strings.TrimSpace(req.ICPNumber)
	}
	if req.ICPLink != "" {
		config.ICPLink = strings.TrimSpace(req.ICPLink)
	}
	if req.PoliceNumber != "" {
		config.PoliceNumber = strings.TrimSpace(req.PoliceNumber)
	}
	if req.PoliceCode != "" {
		config.PoliceCode = strings.TrimSpace(req.PoliceCode)
	}

	if err := db.DB.Save(&config).Error; err != nil {
		Error(c, http.StatusInternalServerError, "更新站点配置失败: "+err.Error())
		return
	}

	Success(c, config)
}

func HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":     "operational",
		"service":    "Loose-Leaf Life Archive Core",
		"runtime":    runtime.Version(),
		"storage_r2": storage.IsConfigured(),
		"time":       time.Now().Unix(),
		"message":    fmt.Sprintf("Archive engine running quietly on %s", runtime.GOARCH),
	})
}
