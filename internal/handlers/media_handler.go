// media_handler.go
// 本机媒体的**受保护**读取路由
//
// 背景：中枢的真实存储是 VPS 本地磁盘，附件原本由 `r.Static("/media", uploadsDir)`
// **无鉴权**地托管 —— 于是任何知道中枢地址的人都能翻阅全部照片与正文，
// 且绕开了「访客仅限换取**已登记**附件签名」这条防签名预言机的规则（BE-21）。
// 对象存储那条路走的是预签名 URL（有签名与有效期），两者口径不一致。
//
// 现在统一为**签名 URL**：
//
//	/media/<object_key>?exp=<unix秒>&sig=<HMAC>
//
// 放行条件（满足其一）：
//   1. 签名有效且未过期
//   2. 携带有效的设备令牌 / 会话 Cookie / 管理员密钥（主人自己的客户端）
//
// 裸路径（无签名）一律拒绝。

package handlers

import (
	"net/http"
	"os"
	"path/filepath"

	"loose-leaf/internal/middleware"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
)

// ServeLocalMedia 提供本机媒体文件（带签名校验）
func ServeLocalMedia(c *gin.Context) {
	rawPath := c.Param("filepath")
	objectKey, ok := storage.LocalMediaPathFromRequest(rawPath)
	if !ok {
		Error(c, http.StatusBadRequest, "非法的媒体路径")
		return
	}

	// 主人自己的客户端（设备令牌 / 会话 Cookie / 管理员密钥）直接放行
	authorized := middleware.IsAdmin(c)

	// 或者：URL 自带有效签名
	if !authorized {
		authorized = storage.VerifyLocalMediaSignature(
			objectKey,
			c.Query("exp"),
			c.Query("sig"),
		)
	}

	if !authorized {
		Error(c, http.StatusUnauthorized, "需要有效签名或授权才能访问媒体")
		return
	}

	base := storage.GetLocalUploadDir()
	if base == "" {
		base = filepath.Join("data", "uploads")
	}
	target := filepath.Join(base, filepath.FromSlash(objectKey))
	if _, err := os.Stat(target); err != nil {
		Error(c, http.StatusNotFound, "媒体不存在")
		return
	}

	// 媒体是私有内容：明确禁止任何中间层缓存
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("X-Content-Type-Options", "nosniff")
	c.File(target)
}
