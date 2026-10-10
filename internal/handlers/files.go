package handlers

import (
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chimu-lab/internal/db"
	"chimu-lab/internal/models"
	"chimu-lab/internal/storage"

	"github.com/gin-gonic/gin"
)

// FileUploadResult 统一文件上传成功后返回给客户端的标准结构
type FileUploadResult struct {
	ObjectKey        string `json:"object_key"`        // R2 存储桶内相对路径 (例如 avatar/user.png, entries/2026/10/09/uuid-file.png)
	URL              string `json:"url"`               // 当前生成的有时效的临时预签名访问 URL (默认 30 分钟)
	ExpiresIn        int    `json:"expires_in"`        // 临时 URL 有效期 (秒)
	Filename         string `json:"filename"`          // 原始文件名
	MimeType         string `json:"mime_type"`         // 检测出的媒体 MIME 类型
	Size             int64  `json:"size"`              // 文件物理大小 (字节)
	Width            int    `json:"width,omitempty"`   // 图片宽度 (像素)
	Height           int    `json:"height,omitempty"`  // 图片高度 (像素)
	AttachmentID     uint   `json:"attachment_id"`     // 已持久化的关联附件 ID (便于 LifeEntry 快速关联)
}

// UnifiedUploadFile 统一文件直传接口 (支持 multipart/form-data)
// 接收参数：
//   - file: 物理文件 (必填)
//   - folder: 目标目录 (可选，如 avatar, entries, photos，默认为 entries)
//   - key / object_key: 自定义相对路径 (可选，如 avatar/user.png)
func UnifiedUploadFile(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		Error(c, http.StatusBadRequest, "缺少待上传文件 (form-data 字段需为 'file')")
		return
	}

	// 50MB 上限安全防范
	maxSize := int64(50 * 1024 * 1024)
	if fileHeader.Size > maxSize {
		Error(c, http.StatusBadRequest, "文件过大，单文件上传限制 50MB")
		return
	}

	folder := strings.TrimSpace(c.DefaultPostForm("folder", "entries"))
	customKey := strings.TrimSpace(c.PostForm("key"))
	if customKey == "" {
		customKey = strings.TrimSpace(c.PostForm("object_key"))
	}

	// 净化并构建规范的存储桶相对路径
	objectKey, err := storage.BuildSafeObjectKey(folder, fileHeader.Filename, customKey)
	if err != nil {
		Error(c, http.StatusBadRequest, "对象相对路径无效: "+err.Error())
		return
	}

	srcFile, err := fileHeader.Open()
	if err != nil {
		Error(c, http.StatusInternalServerError, "打开上传流失败: "+err.Error())
		return
	}
	defer srcFile.Close()

	// 尝试解析图片宽高尺寸 (RC-14)
	var width, height int
	if imgCfg, _, err := image.DecodeConfig(srcFile); err == nil {
		width = imgCfg.Width
		height = imgCfg.Height
	}
	if seeker, ok := srcFile.(io.Seeker); ok {
		_, _ = seeker.Seek(0, io.SeekStart)
	}

	// MIME 探测
	contentType := fileHeader.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		ext := filepath.Ext(fileHeader.Filename)
		if detected := mime.TypeByExtension(ext); detected != "" {
			contentType = detected
		} else {
			contentType = "application/octet-stream"
		}
	}

	// 执行上传流到 R2 (未配 R2 时自动平滑回退本地存储)
	if err := storage.UploadObject(c.Request.Context(), objectKey, srcFile, fileHeader.Size, contentType); err != nil {
		Error(c, http.StatusInternalServerError, "存储端写入失败: "+err.Error())
		return
	}

	// 生成有时效的预签名访问 URL (默认 30 分钟)
	signedURL, expiresIn, _ := storage.GeneratePresignedGetURL(objectKey)

	// 自动在 attachments 数据表中记录该实体，防止成为游离资源
	att := models.Attachment{
		ObjectKey:        objectKey,
		OriginalFilename: fileHeader.Filename,
		MimeType:         contentType,
		Size:             fileHeader.Size,
		Width:            width,
		Height:           height,
	}
	db.DB.Where(models.Attachment{ObjectKey: objectKey}).FirstOrCreate(&att)
	if (att.Width == 0 || att.Height == 0) && (width > 0 && height > 0) {
		db.DB.Model(&att).Updates(map[string]interface{}{"width": width, "height": height})
	}

	Success(c, FileUploadResult{
		ObjectKey:    objectKey,
		URL:          signedURL,
		ExpiresIn:    expiresIn,
		Filename:     fileHeader.Filename,
		MimeType:     contentType,
		Size:         fileHeader.Size,
		Width:        width,
		Height:       height,
		AttachmentID: att.ID,
	})
}

// SignFileURL 获取指定相对路径文件的临时签名预签名 URL (公开只读接口)
// Query 参数：
//   - key / path / object_key: 相对路径 (如 avatar/user.png)
//   - expires: 自定义过期时效 (秒，可选，默认 1800 秒 / 30 分钟)
func SignFileURL(c *gin.Context) {
	key := strings.TrimSpace(c.Query("key"))
	if key == "" {
		key = strings.TrimSpace(c.Query("path"))
	}
	if key == "" {
		key = strings.TrimSpace(c.Query("object_key"))
	}

	if key == "" {
		Error(c, http.StatusBadRequest, "缺少待签名的对象路径 (key/path/object_key)")
		return
	}

	cleanKey := storage.ExtractObjectKey(key)

	var customDuration time.Duration
	if expStr := c.Query("expires"); expStr != "" {
		if sec, err := strconv.Atoi(expStr); err == nil && sec > 0 && sec <= 604800 {
			customDuration = time.Duration(sec) * time.Second
		}
	}

	signedURL, expiresIn, err := storage.GeneratePresignedGetURL(cleanKey, customDuration)
	if err != nil {
		Error(c, http.StatusInternalServerError, "生成临时签名 URL 失败: "+err.Error())
		return
	}

	Success(c, gin.H{
		"object_key": cleanKey,
		"url":        signedURL,
		"expires_in": expiresIn,
	})
}

// BatchSignRequest 批量签名请求载荷
type BatchSignRequest struct {
	Keys      []string `json:"keys" binding:"required"`
	ExpiresIn int      `json:"expires_in"` // 可选，秒
}

// SignFilesBatch 批量换取临时预签名 URL
func SignFilesBatch(c *gin.Context) {
	var req BatchSignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "请求参数无效，请提供 keys 列表")
		return
	}

	var customDuration time.Duration
	if req.ExpiresIn > 0 && req.ExpiresIn <= 604800 {
		customDuration = time.Duration(req.ExpiresIn) * time.Second
	}

	type Item struct {
		ObjectKey string `json:"object_key"`
		URL       string `json:"url"`
		ExpiresIn int    `json:"expires_in"`
	}

	var results []Item
	for _, rawKey := range req.Keys {
		rawKey = strings.TrimSpace(rawKey)
		if rawKey == "" {
			continue
		}
		cleanKey := storage.ExtractObjectKey(rawKey)
		signedURL, exp, err := storage.GeneratePresignedGetURL(cleanKey, customDuration)
		if err != nil {
			signedURL = storage.GetPublicURL(cleanKey)
			exp = 0
		}
		results = append(results, Item{
			ObjectKey: cleanKey,
			URL:       signedURL,
			ExpiresIn: exp,
		})
	}

	Success(c, gin.H{
		"items": results,
	})
}
