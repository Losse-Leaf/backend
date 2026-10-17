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

	_ "golang.org/x/image/webp"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

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

// allowedUploadFolders 统一文件直传允许的归类目录白名单 (与 docs/backend.md §四.5 路径约定一致)
var allowedUploadFolders = map[string]bool{
	"entries": true,
	"avatar":  true,
	"covers":  true,
	"audio":   true,
}

// UnifiedUploadFile 统一文件直传接口 (支持 multipart/form-data)
// 接收参数：
//   - file: 物理文件 (必填)
//   - folder: 目标目录 (可选，取值 entries / avatar / covers / audio，默认 entries)
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

	// 净化并构建规范的存储桶相对路径 (folder 仅允许白名单目录，杜绝任意子目录写入 BE-22)
	folder = strings.TrimSpace(folder)
	if !allowedUploadFolders[folder] {
		Error(c, http.StatusBadRequest, "不支持的存储归类目录")
		return
	}
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

// isRegisteredObjectKey 检查 objectKey 是否属于数据库中已登记的合法生效附件或主理人媒体 (BE-21)
// 访客权限收敛：仅放行已绑定至存活 (未软删) 的 LifeEntry, Wish 或 Footprint 的附件，以及主理人头像
func isRegisteredObjectKey(cleanKey string) bool {
	if cleanKey == "" || db.DB == nil {
		return false
	}

	// 1. 检查主理人头像 (包含默认 avatar/user.png 与自定义头像相对路径)
	if cleanKey == "avatar/user.png" {
		return true
	}
	var profile models.CuratorProfile
	if err := db.DB.Select("avatar_url").Limit(1).Find(&profile).Error; err == nil && profile.AvatarURL != "" {
		if storage.ExtractObjectKey(profile.AvatarURL) == cleanKey || profile.AvatarURL == cleanKey {
			return true
		}
	}

	// 2. 检查 attachments 附件表：必须绑定在有效、未软删除的实体上
	var atts []models.Attachment
	if err := db.DB.Where("object_key = ?", cleanKey).Find(&atts).Error; err == nil && len(atts) > 0 {
		for _, att := range atts {
			if att.EntryID != nil && *att.EntryID > 0 {
				var count int64
				if err := db.DB.Model(&models.LifeEntry{}).Where("id = ?", *att.EntryID).Count(&count).Error; err == nil && count > 0 {
					return true
				}
			}
			if att.WishID != nil && *att.WishID > 0 {
				var count int64
				if err := db.DB.Model(&models.Wish{}).Where("id = ?", *att.WishID).Count(&count).Error; err == nil && count > 0 {
					return true
				}
			}
			if att.FootprintID != nil && *att.FootprintID > 0 {
				var count int64
				if err := db.DB.Model(&models.Footprint{}).Where("id = ?", *att.FootprintID).Count(&count).Error; err == nil && count > 0 {
					return true
				}
			}
		}
	}

	return false
}

// SignFileURL 获取指定相对路径文件的临时签名预签名 URL (公开只读接口)
// 鉴权策略 (BE-21)：已登录管理员可签名任意 key；未认证访客仅可为数据库已登记附件与头像换取签名，阻断签名预言机
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

	// 校验资源归属 (BE-21): 非管理员仅允许签名合法登记的资源
	if !middleware.IsAdmin(c) && !isRegisteredObjectKey(cleanKey) {
		Error(c, http.StatusForbidden, "该资源未在系统中登记，访客无权获取签名")
		return
	}

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

	isAdmin := middleware.IsAdmin(c)
	var results []Item
	for _, rawKey := range req.Keys {
		rawKey = strings.TrimSpace(rawKey)
		if rawKey == "" {
			continue
		}
		cleanKey := storage.ExtractObjectKey(rawKey)

		// 资源归属过滤 (BE-21): 访客请求自动忽略未在数据库登记的游离资源
		if !isAdmin && !isRegisteredObjectKey(cleanKey) {
			continue
		}

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
