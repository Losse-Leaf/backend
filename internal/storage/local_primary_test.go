package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本地为主：写入后文件必须在本地磁盘
func TestLocalPrimaryWritesToLocalDisk(t *testing.T) {
	dir := t.TempDir()
	original := GetLocalUploadDir()
	SetLocalUploadDir(dir)
	defer SetLocalUploadDir(original)

	key := "entries/2026/10/21/local-primary.txt"
	payload := []byte("本地为主")
	if err := UploadObject(context.Background(), key, strings.NewReader(string(payload)), int64(len(payload)), "text/plain"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 本地文件必须存在且内容正确
	localPath := filepath.Join(dir, filepath.FromSlash(key))
	data, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("本地文件缺失: %v", err)
	}
	if string(data) != string(payload) {
		t.Errorf("本地内容应为 %q，实际 %q", payload, data)
	}

	// 附件 URL 应指向中枢自己的 /media 路由（同机直读）
	url := SignResourceURL(key)
	if !strings.HasPrefix(url, "/media/") {
		t.Errorf("本地为主的附件 URL 应为 /media/...，实际 %q", url)
	}
}

// 本地为主：读本地优先
func TestLocalPrimaryReadsLocalFirst(t *testing.T) {
	dir := t.TempDir()
	original := GetLocalUploadDir()
	SetLocalUploadDir(dir)
	defer SetLocalUploadDir(original)

	key := "entries/2026/10/21/read-first.txt"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(key))), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(key)), []byte("本地内容"), 0o644); err != nil {
		t.Fatalf("写本地文件失败: %v", err)
	}

	data, err := ReadObject(context.Background(), key)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "本地内容" {
		t.Errorf("应读到本地内容，实际 %q", data)
	}
}

// 路径越界必须被拒绝
func TestLocalPrimaryRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	original := GetLocalUploadDir()
	SetLocalUploadDir(dir)
	defer SetLocalUploadDir(original)

	for _, key := range []string{"../../etc/passwd", "entries/../../escape.txt"} {
		if path := LocalObjectPath(key); path != "" && !strings.HasPrefix(path, dir) {
			t.Errorf("越界路径应被拒绝: key=%q → %q", key, path)
		}
	}
}

// 存在性判断与缓存失效
func TestLocalPrimaryExistenceCache(t *testing.T) {
	dir := t.TempDir()
	original := GetLocalUploadDir()
	SetLocalUploadDir(dir)
	defer SetLocalUploadDir(original)

	key := "entries/2026/10/21/exists.txt"
	if ObjectExistsLocally(key) {
		t.Errorf("文件尚不存在时不应报告存在")
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(key))), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(key)), []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	// 写入后必须立刻能看出存在（缓存要失效）
	InvalidateLocalObjectCache(key)
	if !ObjectExistsLocally(key) {
		t.Errorf("文件存在时��报告存在")
	}
}
