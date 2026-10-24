package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

func backupR2TestServer(t *testing.T, attempts int, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider("test-access", "test-secret", ""),
		HTTPClient:  server.Client(),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = attempts
				o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
			})
		},
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(server.URL)
		o.UsePathStyle = true
	})
	original := r2ClientsSnapshot()
	s3Mu.Lock()
	s3Client, presignClient = client, s3.NewPresignClient(client)
	r2Bucket, r2PublicDomain, isConfigured = "test-bucket", "", true
	s3Mu.Unlock()
	t.Cleanup(func() {
		s3Mu.Lock()
		s3Client, presignClient = original.client, original.presign
		r2Bucket, r2PublicDomain, isConfigured = original.bucket, original.publicDomain, original.configured
		s3Mu.Unlock()
	})
}

func TestBackupObjectToR2Payloads(t *testing.T) {
	for _, tc := range []struct {
		name, source, contentType string
		offset                    int64
	}{
		{name: "normal", source: "complete payload", contentType: "application/zip"},
		{name: "empty"},
		{name: "offset", source: "ignored:payload tail", offset: 8},
		{name: "at_eof", source: "ignored", offset: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.source[tc.offset:]
			var requests atomic.Int32
			backupR2TestServer(t, 1, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
				if err != nil || string(body) != want || r.ContentLength != int64(len(want)) {
					t.Errorf("payload=%q, length=%d, err=%v; want %q", body, r.ContentLength, err, want)
				}
				contentType := tc.contentType
				if contentType == "" {
					contentType = "application/octet-stream"
				}
				if r.Method != http.MethodPut || r.URL.Path != "/test-bucket/backup.zip" || r.Header.Get("Content-Type") != contentType {
					t.Errorf("unexpected request: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
				}
				if r.Header.Get("Authorization") == "" || r.Header.Get("X-Amz-Content-Sha256") != fmt.Sprintf("%x", sha256.Sum256([]byte(want))) {
					t.Error("missing signature or incorrect payload hash")
				}
				w.WriteHeader(http.StatusOK)
			})
			reader := strings.NewReader(tc.source)
			if _, err := reader.Seek(tc.offset, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if err := BackupObjectToR2(context.Background(), " /backup.zip ", reader, int64(len(want)), tc.contentType); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatalf("requests=%d, want 1", requests.Load())
			}
		})
	}
}

func TestBackupObjectToR2RejectsSizeMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		offset, size int64
	}{
		{name: "negative", source: "abc", size: -1},
		{name: "shorter", source: "abc", size: 4},
		{name: "longer", source: "abc", size: 2},
		{name: "nonempty_declared_empty", source: "abc"},
		{name: "empty_declared_nonempty", size: 1},
		{name: "offset_total_size", source: "prefix:abc", offset: 7, size: 10},
		{name: "past_eof", source: "abc", offset: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			backupR2TestServer(t, 1, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusOK)
			})
			reader := strings.NewReader(tc.source)
			if _, err := reader.Seek(tc.offset, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if err := BackupObjectToR2(context.Background(), "backup.zip", reader, tc.size, ""); err == nil {
				t.Error("size mismatch accepted")
			}
			if requests.Load() != 0 {
				t.Errorf("invalid size sent %d requests", requests.Load())
			}
			if offset, err := reader.Seek(0, io.SeekCurrent); err != nil || offset != tc.offset {
				t.Errorf("offset=%d, err=%v; want restored offset %d", offset, err, tc.offset)
			}
		})
	}
}

func TestBackupObjectToR2LargeFile(t *testing.T) {
	const prefixSize int64 = 512 << 20
	const tail = "tail beyond the old 512 MiB limit"
	const prefix = "PK\x03\x04local source"
	const size = prefixSize + int64(len(tail))
	dir := t.TempDir()
	original := GetLocalUploadDir()
	SetLocalUploadDir(dir)
	t.Cleanup(func() { SetLocalUploadDir(original) })
	path := filepath.Join(dir, "backup.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if _, err := file.WriteAt([]byte(prefix), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte(tail), prefixSize); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	backupR2TestServer(t, 1, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		n, err := io.CopyN(io.Discard, r.Body, prefixSize)
		if err != nil {
			t.Errorf("reading prefix: %v", err)
		}
		end, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		n += int64(len(end))
		if err != nil || n != size || string(end) != tail || r.ContentLength != size {
			t.Errorf("received=%d, Content-Length=%d, tail=%q, err=%v; want %d bytes and %q", n, r.ContentLength, end, err, size, tail)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := BackupObjectToR2(context.Background(), "backup.zip", file, size, "application/zip"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d, want 1", requests.Load())
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != size {
		t.Fatalf("local source size changed: info=%v, err=%v", info, err)
	}
	for offset, want := range map[int64]string{0: prefix, prefixSize: tail} {
		buf := make([]byte, len(want))
		if _, err := file.ReadAt(buf, offset); err != nil || string(buf) != want {
			t.Errorf("local source at %d changed: %q, err=%v", offset, buf, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("backup created local copies: entries=%v, err=%v", entries, err)
	}
}

type backupFaultReader struct {
	io.ReadSeeker
	readErr   error
	seekErr   error
	failSeek  int
	seekCalls int
	reads     int
}

func (r *backupFaultReader) Read(p []byte) (int, error) {
	r.reads++
	if r.readErr != nil {
		return 0, r.readErr
	}
	return r.ReadSeeker.Read(p)
}

func (r *backupFaultReader) Seek(offset int64, whence int) (int64, error) {
	r.seekCalls++
	if r.seekCalls == r.failSeek {
		return 0, r.seekErr
	}
	return r.ReadSeeker.Seek(offset, whence)
}

func TestBackupObjectToR2SourceErrors(t *testing.T) {
	failure := errors.New("injected source failure")
	for _, tc := range []struct {
		name     string
		failSeek int
		readErr  error
	}{
		{name: "read", readErr: failure},
		{name: "seek_current", failSeek: 1},
		{name: "seek_end", failSeek: 2},
		{name: "seek_restore", failSeek: 3},
		{name: "sdk_seek", failSeek: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			backupR2TestServer(t, 1, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusOK)
			})
			reader := &backupFaultReader{ReadSeeker: strings.NewReader("prefix:abc"), failSeek: tc.failSeek, seekErr: failure, readErr: tc.readErr}
			if _, err := reader.ReadSeeker.Seek(7, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if err := BackupObjectToR2(context.Background(), "backup.zip", reader, 3, ""); !errors.Is(err, failure) {
				t.Errorf("error=%v, want source failure", err)
			}
			if requests.Load() != 0 {
				t.Errorf("source failure sent %d requests", requests.Load())
			}
			if tc.failSeek == 2 {
				if offset, err := reader.ReadSeeker.Seek(0, io.SeekCurrent); err != nil || offset != 7 {
					t.Errorf("failed end seek did not restore offset: %d, %v", offset, err)
				}
			}
		})
	}
}

func TestBackupObjectToR2Cancellation(t *testing.T) {
	t.Run("before_read", func(t *testing.T) {
		var requests atomic.Int32
		backupR2TestServer(t, 1, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reader := &backupFaultReader{ReadSeeker: strings.NewReader("abc")}
		if err := BackupObjectToR2(ctx, "backup.zip", reader, 3, ""); !errors.Is(err, context.Canceled) {
			t.Errorf("error=%v, want context.Canceled", err)
		}
		if requests.Load() != 0 || reader.reads != 0 || reader.seekCalls != 0 {
			t.Errorf("cancelled upload performed I/O: requests=%d, reads=%d, seeks=%d", requests.Load(), reader.reads, reader.seekCalls)
		}
	})
	t.Run("in_flight", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var requests atomic.Int32
		backupR2TestServer(t, 2, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			cancel()
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		if err := BackupObjectToR2(ctx, "backup.zip", strings.NewReader("abc"), 3, ""); !errors.Is(err, context.Canceled) {
			t.Errorf("error=%v, want context.Canceled", err)
		}
		if requests.Load() != 1 {
			t.Errorf("requests=%d, want 1", requests.Load())
		}
	})
}

func TestBackupObjectToR2HTTPAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   int32
	}{
		{name: "retry_success", status: http.StatusServiceUnavailable, want: 2},
		{name: "retry_exhausted", status: http.StatusServiceUnavailable, want: 2},
		{name: "forbidden", status: http.StatusForbidden, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const payload = "complete retry payload"
			var requests atomic.Int32
			backupR2TestServer(t, 2, func(w http.ResponseWriter, r *http.Request) {
				attempt := requests.Add(1)
				body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
				if err != nil || string(body) != payload || r.ContentLength != int64(len(payload)) {
					t.Errorf("attempt %d: body=%q, length=%d, err=%v", attempt, body, r.ContentLength, err)
				}
				if r.Header.Get("X-Amz-Content-Sha256") != fmt.Sprintf("%x", sha256.Sum256([]byte(payload))) {
					t.Errorf("attempt %d: incorrect signature payload hash", attempt)
				}
				if tc.name == "retry_success" && attempt == 2 {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "<Error><Code>InjectedFailure</Code><Message>local test failure</Message></Error>")
			})
			reader := strings.NewReader("skip:" + payload)
			if _, err := reader.Seek(5, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			err := BackupObjectToR2(context.Background(), "backup.zip", reader, int64(len(payload)), "")
			if tc.name == "retry_success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "InjectedFailure") {
				t.Errorf("error=%v, want HTTP failure", err)
			}
			if requests.Load() != tc.want {
				t.Errorf("requests=%d, want %d", requests.Load(), tc.want)
			}
		})
	}
}
