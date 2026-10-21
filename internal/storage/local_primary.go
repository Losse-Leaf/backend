// local_primary.go
// 「本地磁盘为主、R2 为备份」的存储策略
//
// 背景：中枢部署在 VPS 上，**真正的存储是 VPS 本地磁盘**，R2 只是备份。
// 早先的实现是「配了 R2 就一切走 R2」—— 上传只写 R2、读取只从 R2 取、
// 附件 URL 一律给 R2 预签名地址，于是：
//   - 客户端每次都要绕道对象存储拉原图（实测 1.5MB / 4.4 秒）
//   - 明明文件就在同一台机器的磁盘上，却要走公网
//
// 现在改成：
//   写入：本地磁盘**为主**（必须成功），R2 作为**备份**顺带写一份（失败不影响主流程）
//   读取：**本地优先**，本地没有才回落 R2
//   URL ：本地有 → 给出中枢自己的 `/media/...` 路由（同机直读，毫秒级）
//         本地没有 → 仍给 R2 预签名 URL（保证历史数据可用）
//
// 开关：环境变量 `LOCAL_MEDIA_PRIMARY`
//   - 缺省 / "1" / "true" → 本地为主（默认）
//   - "0" / "false"        → 退回旧的「R2 为主」行为

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// localPrimaryEnabled 本地为主开关
var localPrimaryEnabled = func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOCAL_MEDIA_PRIMARY"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}()

// LocalPrimaryEnabled 当前是否处于「本地磁盘为主」模式
func LocalPrimaryEnabled() bool { return localPrimaryEnabled }

// 本地存在性缓存：避免每次列附件都做一次 os.Stat
var (
	localExistsMu    sync.RWMutex
	localExistsCache = map[string]localExistsEntry{}
)

type localExistsEntry struct {
	exists   bool
	expireAt time.Time
}

// localExistsTTL 存在性缓存有效期
const localExistsTTL = 60 * time.Second

// LocalObjectPath 返回对象键对应的本地绝对路径（不做存在性检查）
func LocalObjectPath(objectKey string) string {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return ""
	}
	base := GetLocalUploadDir()
	if base == "" {
		base = filepath.Join("data", "uploads")
	}
	target := filepath.Join(base, filepath.FromSlash(objectKey))
	// 兜底断言：解析后的绝对路径必须仍在上传根目录内，杜绝越界访问
	if absBase, err := filepath.Abs(base); err == nil {
		if absTarget, err := filepath.Abs(target); err == nil {
			if absTarget != absBase && !strings.HasPrefix(absTarget, absBase+string(filepath.Separator)) {
				return ""
			}
		}
	}
	return target
}

// ObjectExistsLocally 判断对象是否已存在于本地磁盘
func ObjectExistsLocally(objectKey string) bool {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return false
	}

	localExistsMu.RLock()
	entry, cached := localExistsCache[objectKey]
	localExistsMu.RUnlock()
	if cached && entry.expireAt.After(time.Now()) {
		return entry.exists
	}

	path := LocalObjectPath(objectKey)
	exists := false
	if path != "" {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			exists = true
		}
	}

	localExistsMu.Lock()
	localExistsCache[objectKey] = localExistsEntry{exists: exists, expireAt: time.Now().Add(localExistsTTL)}
	localExistsMu.Unlock()
	return exists
}

// InvalidateLocalObjectCache 让某个对象的存在性缓存失效（写入/删除后调用）
func InvalidateLocalObjectCache(objectKey string) {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return
	}
	localExistsMu.Lock()
	delete(localExistsCache, objectKey)
	localExistsMu.Unlock()
}

// writeLocalObject 写入本地磁盘（返回写入字节数）
func writeLocalObject(objectKey string, reader io.Reader, size int64) error {
	target := LocalObjectPath(objectKey)
	if target == "" {
		return errors.New("对象路径越界，已拒绝写入")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("failed to create upload dir: %w", err)
	}
	outFile, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer outFile.Close()

	written, err := io.Copy(outFile, reader)
	if err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("failed to write local file: %w", err)
	}
	if size > 0 && written != size {
		_ = os.Remove(target)
		return fmt.Errorf("本地写入字节数不符：期望 %d，实际 %d", size, written)
	}
	InvalidateLocalObjectCache(objectKey)
	return nil
}

// readLocalObject 读取本地磁盘上的对象
func readLocalObject(objectKey string) ([]byte, error) {
	target := LocalObjectPath(objectKey)
	if target == "" {
		return nil, errors.New("对象路径越界")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	InvalidateLocalObjectCache(objectKey)
	return data, nil
}

// ReadObject 读取对象字节：**本地优先**，本地没有才回落 R2
//
// 关键：这是「本地为主」策略的读取入口。
// 历史数据可能只存在于 R2（切换策略之前上传的），所以必须有回落，
// 否则老相纸会全部 404。
func ReadObject(ctx context.Context, objectKey string) ([]byte, error) {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return nil, errors.New("object_key 不能为空")
	}

	if localPrimaryEnabled {
		if data, err := readLocalObject(objectKey); err == nil {
			return data, nil
		}
	}

	// 回落：R2
	body, _, err := GetObject(ctx, objectKey)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("对象不存在")
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 256<<20))
}

// MirrorObjectToLocal 从 R2 拉一份对象到本地磁盘
//
// 用途：客户端「预签名直传」直接传到 R2，服务器没经过那份字节；
// 要让「本地为主」成立，就必须在上传确认时把它同步回本地。
func MirrorObjectToLocal(ctx context.Context, objectKey string) error {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return errors.New("object_key 不能为空")
	}
	if ObjectExistsLocally(objectKey) {
		return nil
	}
	body, _, err := GetObject(ctx, objectKey)
	if err != nil {
		return err
	}
	if body == nil {
		return errors.New("R2 中不存在该对象")
	}
	defer body.Close()
	return writeLocalObject(objectKey, body, 0)
}

// BackupObjectToR2 仅写入 R2（不碰本地磁盘）
//
// 用途：把「本地已有 → 备份到 R2」这类**单向备份**动作与「本地为主」的
// UploadObject 区分开。
// 关键：若误用 UploadObject，当源文件就在本地时，它会**先截断再从自己读**
// —— 文件被自己覆盖成空，备份内容全毁。
func BackupObjectToR2(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error {
	objectKey = strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if objectKey == "" {
		return errors.New("object_key 不能为空")
	}
	if !IsConfigured() {
		return errors.New("对象存储未配置")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	payload, err := io.ReadAll(io.LimitReader(reader, 512<<20))
	if err != nil {
		return fmt.Errorf("failed to read payload: %w", err)
	}
	putInput := &s3.PutObjectInput{
		Bucket:        aws.String(r2Bucket),
		Key:           aws.String(objectKey),
		Body:          bytes.NewReader(payload),
		ContentLength: aws.Int64(int64(len(payload))),
		ContentType:   aws.String(contentType),
	}
	_, err = s3Client.PutObject(ctx, putInput)
	return err
}
