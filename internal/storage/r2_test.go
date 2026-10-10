package storage

import (
	"strings"
	"testing"
	"time"

	"loose-leaf/internal/config"
)

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
			{"https://assets.codeactivityhub.top/avatar/user.png?X-Amz-Algorithm=AWS4", "avatar/user.png"},
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
