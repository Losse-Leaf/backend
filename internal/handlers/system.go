package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

// GetSystemStatus 获取系统冷启动与初始化状态探针 (公开 BE-18, BE-19)
func GetSystemStatus(c *gin.Context) {
	var cfg models.SiteConfig
	db.DB.Limit(1).Find(&cfg)

	siteName := cfg.SiteName
	if siteName == "" {
		siteName = "散页 · 个人生活志"
	}

	Success(c, gin.H{
		"initialized":  cfg.Initialized,
		"site_name":    siteName,
		"curator_name": cfg.CuratorName,
	})
}

// UpdateCheckResponse 系统版本更新检查响应结构 (RC-97)
type UpdateCheckResponse struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	HasUpdate      bool   `json:"has_update"`
	ReleaseURL     string `json:"release_url"`
	PublishedAt    string `json:"published_at"`
	CheckedAt      string `json:"checked_at"`
	CheckError     string `json:"check_error,omitempty"`
}

var (
	githubLatestReleaseURL = "https://api.github.com/repos/Losse-Leaf/backend/releases/latest"
	updateCheckCacheTTL    = time.Hour
	updateCheckMu          sync.Mutex
	cachedUpdateCheck      *UpdateCheckResponse
	cachedUpdateCheckAt    time.Time
)

func resetUpdateCheckCache() {
	updateCheckMu.Lock()
	defer updateCheckMu.Unlock()
	cachedUpdateCheck = nil
	cachedUpdateCheckAt = time.Time{}
}

// parseSemver 解析语义化版本号（例如 "v1.2.3" -> [1, 2, 3], true）
func parseSemver(v string) ([3]int, bool) {
	cleaned := strings.TrimSpace(v)
	cleaned = strings.TrimPrefix(strings.TrimPrefix(cleaned, "v"), "V")
	if cleaned == "" || cleaned == "dev" {
		return [3]int{0, 0, 0}, false
	}
	if idx := strings.IndexByte(cleaned, '-'); idx != -1 {
		cleaned = cleaned[:idx]
	}
	if idx := strings.IndexByte(cleaned, '+'); idx != -1 {
		cleaned = cleaned[:idx]
	}
	parts := strings.Split(cleaned, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return [3]int{0, 0, 0}, false
	}
	var res [3]int
	for i := 0; i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return [3]int{0, 0, 0}, false
		}
		res[i] = n
	}
	return res, true
}

// compareSemver 比较两个版本号 a 与 b：若 a > b 返回 1，a < b 返回 -1，相等返回 0
func compareSemver(a, b string) int {
	va, okA := parseSemver(a)
	vb, okB := parseSemver(b)
	if okA && okB {
		for i := 0; i < 3; i++ {
			if va[i] > vb[i] {
				return 1
			}
			if va[i] < vb[i] {
				return -1
			}
		}
		return 0
	}
	if okA && !okB {
		return 1
	}
	if !okA && okB {
		return -1
	}
	return strings.Compare(strings.TrimSpace(a), strings.TrimSpace(b))
}

// isNewerSemver 判断远端 latestVersion 是否比本地 currentVersion 更新
func isNewerSemver(currentVersion, latestVersion string) bool {
	if strings.TrimSpace(latestVersion) == "" {
		return false
	}
	_, okLatest := parseSemver(latestVersion)
	if !okLatest {
		return false
	}
	return compareSemver(latestVersion, currentVersion) > 0
}

// CheckSystemUpdate 检查 GitHub 最新发行版状态（需管理员鉴权，内存级 1 小时缓存，RC-97）
func CheckSystemUpdate(c *gin.Context) {
	force := c.Query("force") == "true" || c.Query("force") == "1"
	currentVer := config.Version
	if strings.TrimSpace(currentVer) == "" {
		currentVer = "dev"
	}

	updateCheckMu.Lock()
	if !force && cachedUpdateCheck != nil && time.Since(cachedUpdateCheckAt) < updateCheckCacheTTL && cachedUpdateCheck.CurrentVersion == currentVer {
		res := *cachedUpdateCheck
		updateCheckMu.Unlock()
		Success(c, res)
		return
	}
	updateCheckMu.Unlock()

	nowStr := time.Now().Format(time.RFC3339)
	defaultReleaseURL := "https://github.com/Losse-Leaf/backend/releases"

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, githubLatestReleaseURL, nil)
	if err != nil {
		Success(c, UpdateCheckResponse{
			CurrentVersion: currentVer,
			LatestVersion:  "",
			HasUpdate:      false,
			ReleaseURL:     defaultReleaseURL,
			PublishedAt:    "",
			CheckedAt:      nowStr,
			CheckError:     "创建版本检查请求失败: " + err.Error(),
		})
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Loose-Leaf-Backend")
	if ghToken := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); ghToken != "" {
		req.Header.Set("Authorization", "Bearer "+ghToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		Success(c, UpdateCheckResponse{
			CurrentVersion: currentVer,
			LatestVersion:  "",
			HasUpdate:      false,
			ReleaseURL:     defaultReleaseURL,
			PublishedAt:    "",
			CheckedAt:      nowStr,
			CheckError:     "无法连接 GitHub Releases 服务: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	// 若仓库未发布 Release 或已转为私有仓库（未认证下 GitHub 均返回 404），给出明确提示而非静默吞下
	if resp.StatusCode == http.StatusNotFound {
		result := UpdateCheckResponse{
			CurrentVersion: currentVer,
			LatestVersion:  "",
			HasUpdate:      false,
			ReleaseURL:     defaultReleaseURL,
			PublishedAt:    "",
			CheckedAt:      nowStr,
			CheckError:     "未检测到可用版本更新",
		}
		updateCheckMu.Lock()
		cachedUpdateCheck = &result
		cachedUpdateCheckAt = time.Now()
		updateCheckMu.Unlock()

		Success(c, result)
		return
	}

	if resp.StatusCode != http.StatusOK {
		Success(c, UpdateCheckResponse{
			CurrentVersion: currentVer,
			LatestVersion:  "",
			HasUpdate:      false,
			ReleaseURL:     defaultReleaseURL,
			PublishedAt:    "",
			CheckedAt:      nowStr,
			CheckError:     fmt.Sprintf("GitHub API 响应异常 (HTTP %d)", resp.StatusCode),
		})
		return
	}

	var ghRelease struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ghRelease); err != nil {
		Success(c, UpdateCheckResponse{
			CurrentVersion: currentVer,
			LatestVersion:  "",
			HasUpdate:      false,
			ReleaseURL:     defaultReleaseURL,
			PublishedAt:    "",
			CheckedAt:      nowStr,
			CheckError:     "解析 GitHub Release 响应失败: " + err.Error(),
		})
		return
	}

	releaseURL := strings.TrimSpace(ghRelease.HTMLURL)
	if releaseURL == "" {
		releaseURL = defaultReleaseURL
	}
	latestVer := strings.TrimSpace(ghRelease.TagName)

	result := UpdateCheckResponse{
		CurrentVersion: currentVer,
		LatestVersion:  latestVer,
		HasUpdate:      isNewerSemver(currentVer, latestVer),
		ReleaseURL:     releaseURL,
		PublishedAt:    strings.TrimSpace(ghRelease.PublishedAt),
		CheckedAt:      nowStr,
	}

	updateCheckMu.Lock()
	cachedUpdateCheck = &result
	cachedUpdateCheckAt = time.Now()
	updateCheckMu.Unlock()

	Success(c, result)
}
