package storage

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// BE-7：/storage/status 的本地统计必须走 60 秒缓存，缓存命中时不得重复 Walk；
// 失效后（上传/删除/迁移）下一次请求才重新统计。
func TestStorageStatusCacheWalksOncePerTTL(t *testing.T) {
	dir := t.TempDir()
	oldDir := GetLocalUploadDir()
	SetLocalUploadDir(dir)
	defer func() {
		SetLocalUploadDir(oldDir)
		InvalidateStorageStatsCache()
	}()

	for _, name := range []string{"a.bin", "sub/b.bin"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	oldWalk := storageWalk
	var walks atomic.Int64
	storageWalk = func(root string, walkFn filepath.WalkFunc) error {
		walks.Add(1)
		return filepath.Walk(root, walkFn)
	}
	defer func() { storageWalk = oldWalk }()

	InvalidateStorageStatsCache()

	status1 := GetStorageStatus()
	status2 := GetStorageStatus()
	if walks.Load() != 1 {
		t.Fatalf("cache hit should walk once, walked %d times", walks.Load())
	}
	if status1["local_file_count"] != int64(2) || status2["local_file_count"] != int64(2) {
		t.Fatalf("unexpected file count: %v / %v", status1["local_file_count"], status2["local_file_count"])
	}
	if status1["local_total_bytes"] != int64(10) {
		t.Fatalf("unexpected total bytes: %v", status1["local_total_bytes"])
	}

	InvalidateStorageStatsCache()
	_ = GetStorageStatus()
	if walks.Load() != 2 {
		t.Fatalf("invalidation should force one fresh walk, total walks=%d", walks.Load())
	}
}

// 并发请求也只会触发一次遍历。
func TestStorageStatusCacheConcurrentSafe(t *testing.T) {
	dir := t.TempDir()
	oldDir := GetLocalUploadDir()
	SetLocalUploadDir(dir)
	defer func() {
		SetLocalUploadDir(oldDir)
		InvalidateStorageStatsCache()
	}()
	if err := os.WriteFile(filepath.Join(dir, "x.bin"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	oldWalk := storageWalk
	var walks atomic.Int64
	storageWalk = func(root string, walkFn filepath.WalkFunc) error {
		walks.Add(1)
		return filepath.Walk(root, walkFn)
	}
	defer func() { storageWalk = oldWalk }()

	InvalidateStorageStatsCache()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = GetStorageStatus()
		}()
	}
	wg.Wait()

	if got := walks.Load(); got != 1 {
		t.Fatalf("concurrent status reads should share one walk, got %d", got)
	}
}
