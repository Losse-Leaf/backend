package storage

import (
	"strings"
	"testing"
	"time"
)

// 媒体签名 URL 的行为约束
//
// 背景：本机媒体原先是无鉴权的静态目录，任何知道中枢地址的人都能翻阅全部照片。
// 现在改为签名 URL（与对象存储的预签名同构），这里钉住它的行为。
func TestLocalMediaSignatureLifecycle(t *testing.T) {
	key := "entries/2026/10/21/test.jpg"

	// 1. 签发出来的 URL 带有效期与签名
	url := SignLocalMediaURL(key)
	if !strings.HasPrefix(url, "/media/") {
		t.Fatalf("签名 URL 应以 /media/ 开头，实际 %q", url)
	}
	if !strings.Contains(url, "exp=") || !strings.Contains(url, "sig=") {
		t.Fatalf("签名 URL 必须带 exp 与 sig，实际 %q", url)
	}

	// 2. 签名有效
	if !VerifyLocalMediaSignature(key, extractParam(url, "exp"), extractParam(url, "sig")) {
		t.Error("刚签发的签名应当有效")
	}

	// 3. 换对象键 → 签名失效（签名与 key 绑定，不能挪用到别的文件）
	otherKey := "entries/2026/10/21/other.jpg"
	if VerifyLocalMediaSignature(otherKey, extractParam(url, "exp"), extractParam(url, "sig")) {
		t.Error("签名不得对其它对象键生效")
	}

	// 4. 篡改签名 → 失效
	if VerifyLocalMediaSignature(key, extractParam(url, "exp"), "deadbeef") {
		t.Error("被篡改的签名必须失效")
	}

	// 5. 缺少参数 → 失效
	if VerifyLocalMediaSignature(key, "", "") {
		t.Error("缺少 exp/sig 时必须失效")
	}

	// 6. 过期 → 失效
	expired := extractParam(url, "exp")
	if VerifyLocalMediaSignature(key, expired, extractParam(url, "sig")) {
		// 用一个已过去的时刻重新签一次，模拟过期
		old := time.Now().Add(-time.Hour).Unix()
		if VerifyLocalMediaSignature(key, itoa64(old), computeMediaSignature(mediaResource(key), old)) {
			t.Error("已过期的签名必须失效")
		}
	}
}

// 缩略图签名与媒体签名不得互相通用
func TestLocalThumbnailSignatureIsSeparate(t *testing.T) {
	key := "entries/2026/10/21/test.jpg"

	thumbURL := SignLocalThumbnailURL(key, 480)
	if !strings.Contains(thumbURL, "/api/v1/files/thumbnail") {
		t.Fatalf("缩略图 URL 路径不对：%q", thumbURL)
	}
	if !VerifyLocalThumbnailSignature(key, 480, extractParam(thumbURL, "exp"), extractParam(thumbURL, "sig")) {
		t.Error("刚签发的缩略图签名应当有效")
	}
	// 缩略图签名不能拿去访问媒体路由
	if VerifyLocalMediaSignature(key, extractParam(thumbURL, "exp"), extractParam(thumbURL, "sig")) {
		t.Error("缩略图签名不得用于媒体路由")
	}
	// 宽度也参与签名
	if VerifyLocalThumbnailSignature(key, 320, extractParam(thumbURL, "exp"), extractParam(thumbURL, "sig")) {
		t.Error("缩略图签名必须与宽度绑定")
	}
}

// 路径穿越必须被挡住
func TestLocalMediaPathTraversalBlocked(t *testing.T) {
	for _, raw := range []string{
		"../../etc/passwd",
		"entries/../../../etc/passwd",
		"/../../secret",
	} {
		if key, ok := LocalMediaPathFromRequest(raw); ok && strings.Contains(key, "..") {
			t.Errorf("越界路径必须被拒绝：%q → %q", raw, key)
		}
	}
	// 正常路径必须通过
	if key, ok := LocalMediaPathFromRequest("entries/2026/10/21/ok.jpg"); !ok || key != "entries/2026/10/21/ok.jpg" {
		t.Errorf("正常路径应通过，实际 %q %v", key, ok)
	}
}

func extractParam(url string, name string) string {
	// 去掉路径部分（第一个 ? 之前）
	if idx := strings.Index(url, "?"); idx >= 0 {
		url = url[idx+1:]
	}
	for _, part := range strings.Split(url, "&") {
		if strings.HasPrefix(part, name+"=") {
			return strings.TrimPrefix(part, name+"=")
		}
	}
	return ""
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
