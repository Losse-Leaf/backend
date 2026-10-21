package handlers

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"
	"loose-leaf/internal/storage"

	"github.com/gin-gonic/gin"
)

// DeriveTitleFromContent 的规则测试
func TestDeriveTitleFromContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"单行", "今天下午的光很好", "今天下午的光很好"},
		{"多行取首行", "第一行\n第二行\n第三行", "第一行"},
		{"首行空白", "   \n第二行", ""},
		{"首尾空白去掉", "  今天的随笔  ", "今天的随笔"},
		{"空内容", "", ""},
		{"超长截断", strings.Repeat("甲", 45), strings.Repeat("甲", 40) + "…"},
		{"恰好不超长", strings.Repeat("甲", 40), strings.Repeat("甲", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveTitleFromContent(tc.content, 40)
			if got != tc.want {
				t.Errorf("DeriveTitleFromContent(%q) = %q, 期望 %q", tc.content, got, tc.want)
			}
		})
	}
}

// 标题为空时，创建散页应自动从正文首行派生
func TestCreateLifeEntryDerivesTitleFromContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTestDB(t)
	// setupTestDB 只迁移了设备/工单/站点配置，这里补上散页与附件表
	if err := db.DB.AutoMigrate(&models.LifeEntry{}, &models.Attachment{}); err != nil {
		t.Fatalf("迁移散页表失败: %v", err)
	}

	// 预置一个管理员设备令牌
	token := "lldt_test_derive_title"
	device := models.Device{
		DeviceID:  "dev_derive_title",
		Name:      "测试设备",
		Platform:  "macos",
		TokenHash: middleware.HashToken(token),
		Status:    "active",
	}
	if err := db.DB.Create(&device).Error; err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	r := gin.New()
	r.POST("/api/v1/entries", middleware.AdminAuthRequired(), CreateLifeEntry)

	body, _ := json.Marshal(map[string]any{
		"type":    "life",
		"title":   "",
		"content": "搬进新居的第一天\n把书架靠到窗边。",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Token", token)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("创建散页失败: %d %s", w.Code, w.Body.String())
	}

	var entry models.LifeEntry
	if err := db.DB.Where("content LIKE ?", "搬进新居的第一天%").First(&entry).Error; err != nil {
		t.Fatalf("未找到刚创建的散页: %v", err)
	}
	if entry.Title != "搬进新居的第一天" {
		t.Errorf("标题应从正文首行派生，实际 %q", entry.Title)
	}
}

// 缩略图路由：应把原图缩到指定宽度并显著变小
func TestThumbnailEndpointDownscales(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 造一张 1600x1200 的 JPEG
	dir := t.TempDir()
	storage.SetLocalUploadDir(dir)
	defer storage.SetLocalUploadDir(filepath.Join("data", "uploads"))

	src := image.NewRGBA(image.Rect(0, 0, 1600, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 1600; x++ {
			src.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, src, nil); err != nil {
		t.Fatalf("构造原图失败: %v", err)
	}
	key := "entries/2026/10/21/test-thumb.jpg"
	target := filepath.Join(dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(target, raw.Bytes(), 0o644); err != nil {
		t.Fatalf("写原图失败: %v", err)
	}

	r := gin.New()
	r.GET("/api/v1/files/thumbnail", ThumbnailFile)

	// 关键：缩略图地址也**必须签名** ——
	// 否则它就是一个公开的「按 key 取图」接口。
	signedURL := storage.SignLocalThumbnailURL(key, 320)
	req := httptest.NewRequest(http.MethodGet, signedURL, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 对照：裸路径（无签名）必须被拒绝
	plainReq := httptest.NewRequest(http.MethodGet, "/api/v1/files/thumbnail?key="+key+"&w=320", nil)
	plainW := httptest.NewRecorder()
	r.ServeHTTP(plainW, plainReq)
	if plainW.Code != http.StatusUnauthorized {
		t.Errorf("无签名的缩略图请求应当被拒绝，实际 %d", plainW.Code)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("缩略图接口失败: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type 应为 image/jpeg，实际 %q", ct)
	}
	thumb, _, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("缩略图无法解码: %v", err)
	}
	bounds := thumb.Bounds()
	if bounds.Dx() != 320 {
		t.Errorf("缩略图宽度应为 320，实际 %d", bounds.Dx())
	}
	// 缩略图必须显著小于原图
	if w.Body.Len() >= raw.Len() {
		t.Errorf("缩略图 (%d B) 应显著小于原图 (%d B)", w.Body.Len(), raw.Len())
	}
}
