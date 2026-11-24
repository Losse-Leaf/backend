package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var alistHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
}

func getAlistConfig() (baseURL, token, mountPath string, enabled bool) {
	baseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("ALIST_BASE_URL")), "/")
	token = strings.TrimSpace(os.Getenv("ALIST_TOKEN"))
	mountPath = strings.TrimRight(strings.TrimSpace(os.Getenv("ALIST_MOUNT_PATH")), "/")
	if mountPath == "" {
		mountPath = "/space/looseleaf"
	} else if !strings.HasPrefix(mountPath, "/") {
		mountPath = "/" + mountPath
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:5244"
	}
	enabled = token != ""
	return
}

// AlistMirrorEnabled 返回当前是否已启用同机 AList (5TB Google Drive) 镜像与透明回源。
func AlistMirrorEnabled() bool {
	_, _, _, ok := getAlistConfig()
	return ok
}

// AlistMountPath 返回当前配置的 AList 挂载基础路径（默认 /space/looseleaf）。
func AlistMountPath() string {
	_, _, mp, _ := getAlistConfig()
	return mp
}

// EscapeObjectKeyPath 对对象键逐段 URL 编码（保留 / 分隔符）。
func EscapeObjectKeyPath(objectKey string) string {
	return escapeObjectKeyPath(objectKey)
}

// MirrorLocalToAlist 将已落盘的本地对象流式镜像至同机 AList 挂载点（如 /space/looseleaf/entries/...）。
func MirrorLocalToAlist(ctx context.Context, objectKey, contentType string) (string, error) {
	baseURL, token, mountPath, ok := getAlistConfig()
	if !ok {
		return "", nil
	}
	cleanKey := strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if cleanKey == "" {
		return "", nil
	}
	localPath := LocalObjectPath(cleanKey)
	if localPath == "" {
		return "", fmt.Errorf("invalid local object key: %s", cleanKey)
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}

	alistPath := mountPath + "/" + cleanKey
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/api/fs/put", f)
	if err != nil {
		return "", err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("File-Path", url.PathEscape(alistPath))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("As-Task", "false")
	req.ContentLength = stat.Size()

	resp, err := alistHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("AList put status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var res struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && res.Code != 0 && res.Code != 200 {
		return "", fmt.Errorf("AList put code %d: %s", res.Code, res.Message)
	}
	return alistPath, nil
}

// MirrorLocalToAlistAsync 后台异步将本地对象镜像至 AList 5TB 云盘。
func MirrorLocalToAlistAsync(objectKey, contentType string) {
	if !AlistMirrorEnabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := MirrorLocalToAlist(ctx, objectKey, contentType); err != nil {
			log.Printf("[storage] AList 5TB 镜像备份失败（本地已写入成功）：key=%s err=%v", objectKey, err)
		}
	}()
}

// FetchFromAlistAndCacheLocal 当本地文件缺失时，透明从同机 AList 拉回并写入本地 SSD 缓存。
func FetchFromAlistAndCacheLocal(ctx context.Context, objectKey string) ([]byte, error) {
	baseURL, token, mountPath, ok := getAlistConfig()
	if !ok {
		return nil, fmt.Errorf("AList mirror not configured")
	}
	cleanKey := strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if cleanKey == "" {
		return nil, fmt.Errorf("empty objectKey")
	}
	alistPath := mountPath + "/" + cleanKey
	targetURL := baseURL + "/d/" + escapeObjectKeyPath(alistPath)

	// 优先调用 /api/fs/get 获取带签名的下载链接（兼容 AList 开启 sign_all: true 的情况）
	getPayload, _ := json.Marshal(map[string]any{"path": alistPath, "password": ""})
	if getReq, gErr := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/fs/get", bytes.NewReader(getPayload)); gErr == nil {
		getReq.Header.Set("Content-Type", "application/json")
		if token != "" {
			getReq.Header.Set("Authorization", token)
		}
		if getResp, doErr := alistHTTPClient.Do(getReq); doErr == nil {
			rawMeta, _ := io.ReadAll(io.LimitReader(getResp.Body, 64*1024))
			_ = getResp.Body.Close()
			var meta struct {
				Code int `json:"code"`
				Data struct {
					Sign   string `json:"sign"`
					RawURL string `json:"raw_url"`
				} `json:"data"`
			}
			if getResp.StatusCode == http.StatusOK && json.Unmarshal(rawMeta, &meta) == nil && meta.Code == 200 {
				if sign := strings.TrimSpace(meta.Data.Sign); sign != "" {
					targetURL = baseURL + "/d/" + escapeObjectKeyPath(alistPath) + "?sign=" + url.QueryEscape(sign)
				} else if rawURL := strings.TrimSpace(meta.Data.RawURL); rawURL != "" {
					targetURL = rawURL
				}
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := alistHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AList fetch HTTP %d", resp.StatusCode)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") || strings.Contains(ct, "text/html") {
		return nil, fmt.Errorf("AList returned unexpected content-type %q", ct)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("AList returned empty object")
	}
	_ = writeLocalObject(cleanKey, bytes.NewReader(data), int64(len(data)))
	return data, nil
}

// DeleteFromAlist 从同机 AList 挂载点删除对应对象（物理 purge 时调用）。
func DeleteFromAlist(ctx context.Context, objectKey string) error {
	baseURL, token, mountPath, ok := getAlistConfig()
	if !ok {
		return nil
	}
	cleanKey := strings.TrimPrefix(strings.TrimSpace(objectKey), "/")
	if cleanKey == "" {
		return nil
	}
	fullPath := mountPath + "/" + cleanKey
	slash := strings.LastIndexByte(fullPath, '/')
	if slash <= 0 || slash == len(fullPath)-1 {
		return nil
	}
	dir := fullPath[:slash]
	name := fullPath[slash+1:]
	payload, _ := json.Marshal(map[string]any{
		"dir":   dir,
		"names": []string{name},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/fs/remove", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := alistHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
