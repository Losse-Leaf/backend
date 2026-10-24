package storage

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/config"
)

func TestPresignCacheDoesNotServeNearlyExpiredURLs(t *testing.T) {
	original := R2ConfigSnapshot()
	t.Cleanup(func() {
		_ = InitR2(original)
		ClearPresignCache()
	})
	if err := InitR2(config.R2Config{
		AccessKeyID: "test-access", SecretAccessKey: "test-secret",
		BucketName: "test-bucket", Endpoint: "http://127.0.0.1:1", PresignExpireSeconds: 1800,
	}); err != nil {
		t.Fatalf("初始化测试对象存储失败: %v", err)
	}
	ClearPresignCache()

	const key = "entries/presign-cache-test.png"
	_, firstExpires, err := GeneratePresignedGetURL(key)
	if err != nil {
		t.Fatalf("首次签名失败: %v", err)
	}
	if firstExpires != 1800 {
		t.Fatalf("首次签名有效期 = %d, want 1800", firstExpires)
	}

	cacheKey := fmt.Sprintf("%s|%d", key, firstExpires)
	presignCacheMu.Lock()
	item, ok := presignCache[cacheKey]
	if !ok {
		presignCacheMu.Unlock()
		t.Fatal("签名缓存未写入")
	}
	item.expiresAt = time.Now().Add(10 * time.Minute)
	presignCache[cacheKey] = item
	presignCacheMu.Unlock()

	_, secondExpires, err := GeneratePresignedGetURL(key)
	if err != nil {
		t.Fatalf("二次签名失败: %v", err)
	}
	if secondExpires < 1700 {
		t.Fatalf("剩余有效期不足 ETag 窗口时必须重新签发，实际剩余 %d 秒", secondExpires)
	}
}

func TestStorageHelpers(t *testing.T) {
	// Initialize with empty config (unconfigured mode)
	_ = InitR2(config.R2Config{
		PresignExpireSeconds: 1800,
	})

	t.Run("ExtractObjectKey from various paths and URLs", func(t *testing.T) {
		tests := []struct {
			input    string
			expected string
		}{
			{"avatar/user.png", "avatar/user.png"},
			{"/avatar/user.png", "avatar/user.png"},
			{"entries/2026/10/09/uuid-shot.jpg", "entries/2026/10/09/uuid-shot.jpg"},
			{"entries/shot%20one.jpg", "entries/shot%20one.jpg"},
			{"https://media.example.com/avatar/user.png?X-Amz-Algorithm=AWS4", "avatar/user.png"},
			{"https://images.unsplash.com/photo-12345?auto=format", "https://images.unsplash.com/photo-12345?auto=format"},
		}

		for _, tc := range tests {
			got := ExtractObjectKey(tc.input)
			if got != tc.expected {
				t.Errorf("ExtractObjectKey(%q) = %q, expected %q", tc.input, got, tc.expected)
			}
		}
	})

	t.Run("BuildSafeObjectKey safety and formatting", func(t *testing.T) {
		// Custom key
		k1, err := BuildSafeObjectKey("avatar", "pic.png", "avatar/user.png")
		if err != nil || k1 != "avatar/user.png" {
			t.Errorf("expected avatar/user.png, got %q, err=%v", k1, err)
		}

		// Traversal attack prevention
		_, err = BuildSafeObjectKey("avatar", "pic.png", "../../etc/passwd")
		if err == nil {
			t.Errorf("expected error for path traversal attack, got nil")
		}

		// Avatar folder default
		k2, err := BuildSafeObjectKey("avatar", "my photo.png", "")
		if err != nil || !strings.HasPrefix(k2, "avatar/") {
			t.Errorf("expected avatar prefix, got %q", k2)
		}

		// Entries default
		k3, err := BuildSafeObjectKey("entries", "shot.jpg", "")
		if err != nil || !strings.HasPrefix(k3, "entries/") {
			t.Errorf("expected entries prefix, got %q", k3)
		}
	})

	t.Run("Default presign duration is 30 minutes", func(t *testing.T) {
		dur := GetPresignExpireDuration()
		if dur != 30*time.Minute {
			t.Errorf("expected 30m, got %v", dur)
		}
	})

	t.Run("SignResourceURL returns fallback URL in offline mode", func(t *testing.T) {
		url := SignResourceURL("avatar/user.png")
		if !strings.Contains(url, "avatar/user.png") {
			t.Errorf("expected avatar/user.png in signed url, got %q", url)
		}
	})

	t.Run("Presign cache idempotency and stability (BE-04)", func(t *testing.T) {
		url1 := SignResourceURL("avatar/user.png")
		url2 := SignResourceURL("avatar/user.png")
		if url1 != url2 {
			t.Errorf("expected consecutive SignResourceURL calls to return identical URL, got %q vs %q", url1, url2)
		}
	})
}
