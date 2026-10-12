package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
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
		siteName = "散页 · 时光手账与生活档案"
	}

	Success(c, gin.H{
		"initialized":      cfg.Initialized,
		"has_passcode_set": strings.TrimSpace(cfg.MasterPasscode) != "",
		"site_name":        siteName,
		"curator_name":     cfg.CuratorName,
	})
}

type BootstrapRequest struct {
	CuratorName     string `json:"curator_name" binding:"required"`
	Location        string `json:"location"`
	MasterPasscode  string `json:"master_passcode"`
	ConfirmPasscode string `json:"confirm_passcode"`
	HubURL          string `json:"hub_url"`
}

// BootstrapSystem 发起首次装帧：双次确认密钥并签发首台设备配对工单 (BE-18, BE-19)
// 注意：本接口仅保存主理人信息与密钥 SHA-256 哈希，保持 initialized = false；
// 需待首台设备调用 POST /api/v1/auth/pair/confirm 完成握手后，才将 initialized 置为 true。
func BootstrapSystem(c *gin.Context) {
	var cfg models.SiteConfig
	db.DB.Limit(1).Find(&cfg)

	if cfg.Initialized {
		Error(c, http.StatusForbidden, "自留地已完成初次装帧，禁止二次重复初始化")
		return
	}
	if strings.TrimSpace(cfg.MasterPasscode) != "" {
		Error(c, http.StatusForbidden, "自留地已设定管理员密钥，配对激活前禁止覆盖初始化")
		return
	}

	var req BootstrapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "缺少必要的主理人初始信息")
		return
	}

	masterPasscode := strings.TrimSpace(req.MasterPasscode)
	confirmPasscode := strings.TrimSpace(req.ConfirmPasscode)
	if masterPasscode == "" {
		Error(c, http.StatusBadRequest, "请设定管理员密钥")
		return
	}
	if masterPasscode != confirmPasscode {
		Error(c, http.StatusBadRequest, "两次输入的管理员密钥不一致")
		return
	}

	curatorName := strings.TrimSpace(req.CuratorName)
	location := strings.TrimSpace(req.Location)

	// 1. 保存 SiteConfig (此时保持 Initialized = false，等待首台设备配对激活 BE-19)
	if cfg.ID == 0 {
		db.DB.FirstOrCreate(&cfg)
	}
	if cfg.SiteName == "" {
		cfg.SiteName = "散页 · 时光手账与生活档案"
	}
	cfg.CuratorName = curatorName
	cfg.DefaultLocation = location
	hashedPasscode, err := HashPasscode(masterPasscode)
	if err != nil {
		Error(c, http.StatusInternalServerError, "加密管理员密钥失败: "+err.Error())
		return
	}
	cfg.MasterPasscode = hashedPasscode
	cfg.Initialized = false
	cfg.UpdatedAt = time.Now()
	if err := db.DB.Save(&cfg).Error; err != nil {
		Error(c, http.StatusInternalServerError, "保存站点配置失败: "+err.Error())
		return
	}

	// 2. 初始化 CuratorProfile (未填写的字段保持纯净空字符串或空数组 RC-52)
	var profile models.CuratorProfile
	db.DB.FirstOrCreate(&profile)
	profile.Name = curatorName
	profile.Location = location
	profile.CurrentCity = location
	if strings.TrimSpace(profile.SocialLinks) == "" {
		profile.SocialLinks = "{}"
	}
	if strings.TrimSpace(profile.EDCGear) == "" {
		profile.EDCGear = "[]"
	}
	profile.UpdatedAt = time.Now()
	db.DB.Save(&profile)

	// 3. 结合前端上报的 hub_url 签发 5 分钟有效的首台设备配对工单 (BE-19, BE-20)
	hubURL := strings.TrimSpace(req.HubURL)
	if hubURL == "" {
		hubURL = resolveHubURL(c)
	}
	hubURL = strings.TrimRight(hubURL, "/")

	ticketStr := fmt.Sprintf("pair_tk_%s", generateRandomHex(6))
	pinCode := generatePinCode()
	expiresIn := 300 // 5 分钟
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	ticket := models.PairTicket{
		Ticket:    ticketStr,
		PinCode:   pinCode,
		HubURL:    hubURL,
		ExpiresAt: expiresAt,
		Used:      false,
	}
	if err := db.DB.Create(&ticket).Error; err != nil {
		Error(c, http.StatusInternalServerError, "生成配对工单失败: "+err.Error())
		return
	}

	qrPayload := fmt.Sprintf("looseleaf://pair?hub=%s&ticket=%s", hubURL, ticketStr)
	qrDataURL := generateQRCodeDataURL(qrPayload)

	Success(c, gin.H{
		"curator_name":       curatorName,
		"location":           location,
		"ticket":             ticketStr,
		"pin_code":           pinCode,
		"hub_url":            hubURL,
		"qr_payload":         qrPayload,
		"qr_code_data_url":   qrDataURL,
		"expires_at":         expiresAt.Format(time.RFC3339),
		"expires_in_seconds": expiresIn,
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
			CheckError:     "未检测到 Release 发行版（若仓库已设为私有，请配置 GITHUB_TOKEN 进行鉴权）",
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

// UpsertUpdateNoticeRequest 新增或修改版本更新说明请求体 (RC-97)
type UpsertUpdateNoticeRequest struct {
	Version    string `json:"version"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	ReleasedAt string `json:"released_at"`
}

// GetUpdateNotices 获取历史版本更新说明列表，按版本号倒序排列 (需管理员鉴权，RC-97)
func GetUpdateNotices(c *gin.Context) {
	var notices []models.UpdateNotice
	if err := db.DB.Order("id DESC").Find(&notices).Error; err != nil {
		Error(c, http.StatusInternalServerError, "查询版本更新说明失败: "+err.Error())
		return
	}
	if notices == nil {
		notices = []models.UpdateNotice{}
	}

	sort.SliceStable(notices, func(i, j int) bool {
		cmp := compareSemver(notices[i].Version, notices[j].Version)
		if cmp != 0 {
			return cmp > 0
		}
		if notices[i].ReleasedAt != notices[j].ReleasedAt {
			return notices[i].ReleasedAt > notices[j].ReleasedAt
		}
		return notices[i].ID > notices[j].ID
	})

	Success(c, notices)
}

// CreateUpdateNotice 录入新版本更新说明 (需管理员鉴权，RC-97)
func CreateUpdateNotice(c *gin.Context) {
	var req UpsertUpdateNoticeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "无效的更新说明参数: "+err.Error())
		return
	}

	version := strings.TrimSpace(req.Version)
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	releasedAt := strings.TrimSpace(req.ReleasedAt)

	if version == "" || title == "" || body == "" {
		Error(c, http.StatusBadRequest, "版本号、更新标题与更新内容不能为空")
		return
	}
	if releasedAt == "" {
		releasedAt = time.Now().Format("2006-01-02")
	}

	notice := models.UpdateNotice{
		Version:    version,
		Title:      title,
		Body:       body,
		ReleasedAt: releasedAt,
	}
	if err := db.DB.Create(&notice).Error; err != nil {
		Error(c, http.StatusInternalServerError, "创建版本更新说明失败: "+err.Error())
		return
	}

	Success(c, notice)
}

// UpdateUpdateNotice 编辑既有版本更新说明 (需管理员鉴权，RC-97)
func UpdateUpdateNotice(c *gin.Context) {
	id := c.Param("id")
	var notice models.UpdateNotice
	if err := db.DB.First(&notice, id).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该版本更新说明")
		return
	}

	var req UpsertUpdateNoticeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, "无效的更新说明参数: "+err.Error())
		return
	}

	if v := strings.TrimSpace(req.Version); v != "" {
		notice.Version = v
	}
	if t := strings.TrimSpace(req.Title); t != "" {
		notice.Title = t
	}
	if b := strings.TrimSpace(req.Body); b != "" {
		notice.Body = b
	}
	if r := strings.TrimSpace(req.ReleasedAt); r != "" {
		notice.ReleasedAt = r
	}

	if strings.TrimSpace(notice.Version) == "" || strings.TrimSpace(notice.Title) == "" || strings.TrimSpace(notice.Body) == "" {
		Error(c, http.StatusBadRequest, "版本号、更新标题与更新内容不能为空")
		return
	}

	if err := db.DB.Save(&notice).Error; err != nil {
		Error(c, http.StatusInternalServerError, "保存版本更新说明失败: "+err.Error())
		return
	}

	Success(c, notice)
}

// DeleteUpdateNotice 删除指定版本更新说明 (需管理员鉴权，RC-97)
func DeleteUpdateNotice(c *gin.Context) {
	id := c.Param("id")
	var notice models.UpdateNotice
	if err := db.DB.First(&notice, id).Error; err != nil {
		Error(c, http.StatusNotFound, "未找到该版本更新说明")
		return
	}

	if err := db.DB.Delete(&notice).Error; err != nil {
		Error(c, http.StatusInternalServerError, "删除版本更新说明失败: "+err.Error())
		return
	}

	Success(c, gin.H{"deleted": true, "id": notice.ID})
}

