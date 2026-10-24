package handlers

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"loose-leaf/internal/backup"
	"loose-leaf/internal/db"

	"github.com/gin-gonic/gin"
)

// finalizeRestore 在数据库生命周期读锁内刷新恢复后的内存缓存与存储配置，
// 避免与并发发起的第二次恢复的替换窗口交错。
func finalizeRestore() {
	db.LockRead()
	defer db.UnlockRead()
	InvalidateStatsCache()
	LoadStorageConfigFromDB()
}

// CreateBackupNow 主理人手动立即触发一次全量备份 (POST /api/v1/system/backup/now)
func CreateBackupNow(c *gin.Context) {
	item, err := backup.CreateBackup(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, fmt.Sprintf("生成全量备份失败: %v", err))
		return
	}
	Success(c, item)
}

// GetBackupList 获取当前对象存储与本地保留的备份清单 (GET /api/v1/system/backup/list)
func GetBackupList(c *gin.Context) {
	items, err := backup.ListBackups(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, fmt.Sprintf("获取备份列表失败: %v", err))
		return
	}
	Success(c, items)
}

// DownloadBackup 下载指定备份压缩包 (GET /api/v1/system/backup/download/:filename)
func DownloadBackup(c *gin.Context) {
	filename := c.Param("filename")
	filename = filepath.Base(strings.TrimSpace(filename))

	reader, size, err := backup.GetBackupStream(c.Request.Context(), filename)
	if err != nil {
		Error(c, http.StatusNotFound, fmt.Sprintf("下载备份失败: %v", err))
		return
	}
	defer reader.Close()

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Header("Content-Type", "application/zip")
	if size > 0 {
		c.Header("Content-Length", fmt.Sprintf("%d", size))
	}

	c.DataFromReader(http.StatusOK, size, "application/zip", reader, nil)
}

// DeleteBackup 删除指定备份快照 (DELETE /api/v1/system/backup/:filename)
func DeleteBackup(c *gin.Context) {
	filename := c.Param("filename")
	filename = filepath.Base(strings.TrimSpace(filename))

	if err := backup.DeleteBackup(c.Request.Context(), filename); err != nil {
		Error(c, http.StatusBadRequest, "删除备份失败: "+err.Error())
		return
	}

	items, _ := backup.ListBackups(c.Request.Context())
	Success(c, items)
}

type RestoreBackupRequest struct {
	Filename string `json:"filename"`
}

// RestoreBackup 执行备份整库恢复 (POST /api/v1/system/backup/restore)
// 支持两种模式：
// 1. JSON 请求：提供已有的备份文件名 { "filename": "looseleaf-backup-..." }，从 R2/本地归档一键回滚
// 2. Multipart 请求：上传本地 .zip 压缩包文件一键导入还原
func RestoreBackup(c *gin.Context) {
	contentType := c.GetHeader("Content-Type")

	// 模式 1：上传本地 zip 恢复
	if strings.Contains(contentType, "multipart/form-data") {
		fileHeader, err := c.FormFile("file")
		if err != nil {
			Error(c, http.StatusBadRequest, "缺少上传的备份压缩包文件 (file)")
			return
		}

		if !strings.HasSuffix(strings.ToLower(fileHeader.Filename), ".zip") {
			Error(c, http.StatusBadRequest, "仅支持恢复 .zip 格式的散页归档文件")
			return
		}

		srcFile, err := fileHeader.Open()
		if err != nil {
			Error(c, http.StatusInternalServerError, "打开上传文件失败")
			return
		}
		defer srcFile.Close()

		tempFile, err := os.CreateTemp("", "looseleaf-upload-restore-*.zip")
		if err != nil {
			Error(c, http.StatusInternalServerError, "创建临时文件失败")
			return
		}
		tempPath := tempFile.Name()
		defer os.Remove(tempPath)

		size, err := io.Copy(tempFile, srcFile)
		tempFile.Close()
		if err != nil {
			Error(c, http.StatusInternalServerError, "读取上传内容失败")
			return
		}

		zipReader, err := zip.OpenReader(tempPath)
		if err != nil {
			Error(c, http.StatusBadRequest, fmt.Sprintf("无效的 ZIP 压缩包: %v", err))
			return
		}
		defer zipReader.Close()

		if err := backup.RestoreFromZipStream(&zipReader.Reader); err != nil {
			Error(c, http.StatusInternalServerError, fmt.Sprintf("恢复备份失败: %v", err))
			return
		}
		finalizeRestore()

		Success(c, gin.H{
			"message":  "已成功从上传的压缩包完整恢复数据库与媒体资源",
			"filename": fileHeader.Filename,
			"size":     size,
		})
		return
	}

	// 模式 2：指定现有备份文件名回滚
	var req RestoreBackupRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Filename) == "" {
		Error(c, http.StatusBadRequest, "请提供要恢复的备份文件名 (filename)")
		return
	}

	filename := filepath.Base(strings.TrimSpace(req.Filename))
	reader, size, err := backup.GetBackupStream(c.Request.Context(), filename)
	if err != nil {
		Error(c, http.StatusNotFound, fmt.Sprintf("未找到待恢复的备份文件: %v", err))
		return
	}
	defer reader.Close()

	tempFile, err := os.CreateTemp("", "looseleaf-remote-restore-*.zip")
	if err != nil {
		Error(c, http.StatusInternalServerError, "创建临时恢复文件失败")
		return
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	if _, err := io.Copy(tempFile, reader); err != nil {
		tempFile.Close()
		Error(c, http.StatusInternalServerError, "拉取备份内容失败")
		return
	}
	tempFile.Close()

	zipReader, err := zip.OpenReader(tempPath)
	if err != nil {
		Error(c, http.StatusBadRequest, fmt.Sprintf("无效的 ZIP 压缩包: %v", err))
		return
	}
	defer zipReader.Close()

	if err := backup.RestoreFromZipStream(&zipReader.Reader); err != nil {
		Error(c, http.StatusInternalServerError, fmt.Sprintf("恢复备份失败: %v", err))
		return
	}
	finalizeRestore()

	Success(c, gin.H{
		"message":  fmt.Sprintf("已成功恢复备份快照 %s", filename),
		"filename": filename,
		"size":     size,
	})
}
