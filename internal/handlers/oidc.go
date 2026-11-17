package handlers

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"loose-leaf/internal/backup"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

//go:embed templates/oidc_bridge.html
var oidcNativeBridgeHTML string

var oidcNativeBridgeTmpl = template.Must(template.New("oidc_native_bridge").Parse(oidcNativeBridgeHTML))

// OIDCLogin 发起标准 OAuth 2.0 / OIDC + PKCE (S256) 授权跳转。
// GET /api/v1/auth/oidc/login
func OIDCLogin(c *gin.Context) {
	oidcCfg := config.AppConfig.OIDC
	if !oidcCfg.Enabled {
		Error(c, http.StatusNotFound, "未启用统一单点登录")
		return
	}
	if strings.TrimSpace(oidcCfg.Issuer) == "" || strings.TrimSpace(oidcCfg.ClientID) == "" {
		Error(c, http.StatusServiceUnavailable, "OIDC 配置不完整，请检查 OIDC_ISSUER 与 OIDC_CLIENT_ID")
		return
	}

	platform := strings.ToLower(strings.TrimSpace(c.Query("platform")))
	if platform == "" {
		platform = "web"
	}
	if platform != "web" && platform != "macos" && platform != "android" {
		Error(c, http.StatusBadRequest, "不支持的客户端平台参数 platform")
		return
	}

	safeReturnTo, ok := validateSafeReturnTo(c.Query("return_to"))
	if !ok {
		Error(c, http.StatusBadRequest, "非法的站内跳转路径 return_to")
		return
	}

	discovery, err := fetchOIDCDiscovery(oidcCfg.Issuer)
	if err != nil {
		Error(c, http.StatusBadGateway, "无法获取身份认证中心配置: "+err.Error())
		return
	}

	state, err := generateRandomBase64URL(32)
	if err != nil {
		Error(c, http.StatusInternalServerError, "生成安全登录状态失败")
		return
	}
	cookieBinding, err := generateRandomBase64URL(32)
	if err != nil {
		Error(c, http.StatusInternalServerError, "生成会话绑定令牌失败")
		return
	}
	nonce, err := generateRandomBase64URL(32)
	if err != nil {
		Error(c, http.StatusInternalServerError, "生成 OIDC nonce 失败")
		return
	}
	codeVerifier, err := generateRandomBase64URL(32)
	if err != nil {
		Error(c, http.StatusInternalServerError, "生成 PKCE 校验器失败")
		return
	}

	redirectURI := resolveOIDCRedirectURI(c, oidcCfg)
	now := oidcStates.now()
	oidcStates.Put(oidcStateEntry{
		State:         state,
		CookieBinding: cookieBinding,
		Nonce:         nonce,
		CodeVerifier:  codeVerifier,
		RedirectURI:   redirectURI,
		Platform:      platform,
		DeviceName:    sanitizeDeviceLabel(c.Query("device_name"), 80),
		DeviceModel:   sanitizeDeviceLabel(c.Query("device_model"), 80),
		ReturnTo:      safeReturnTo,
		CreatedAt:     now,
		ExpiresAt:     now.Add(oidcStateTTL),
	})

	setOIDCBindingCookie(c, cookieBinding)

	authURL, err := url.Parse(discovery.AuthorizationEndpoint)
	if err != nil {
		Error(c, http.StatusBadGateway, "身份认证中心授权地址格式无效")
		return
	}
	q := authURL.Query()
	q.Set("client_id", oidcCfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "openid profile email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", computePKCES256Challenge(codeVerifier))
	q.Set("code_challenge_method", "S256")
	authURL.RawQuery = q.Encode()

	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, authURL.String())
}

// OIDCCallback 处理 Vesper Auth OIDC 授权码回调，验签 ID Token 并嫁接既有受信任设备体系。
// GET /api/v1/auth/oidc/callback
func OIDCCallback(c *gin.Context) {
	oidcCfg := config.AppConfig.OIDC
	if !oidcCfg.Enabled {
		Error(c, http.StatusNotFound, "未启用统一单点登录")
		return
	}

	cookieVal, _ := c.Cookie(oidcBindingCookieName)
	clearOIDCBindingCookie(c)

	if oauthErr := strings.TrimSpace(c.Query("error")); oauthErr != "" {
		Error(c, http.StatusBadRequest, "身份认证未完成或已被取消: "+oauthErr)
		return
	}

	state := strings.TrimSpace(c.Query("state"))
	code := strings.TrimSpace(c.Query("code"))
	entry, err := oidcStates.Consume(state, strings.TrimSpace(cookieVal))
	if err != nil {
		Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if code == "" {
		Error(c, http.StatusBadRequest, "缺少授权码参数 code")
		return
	}

	discovery, err := fetchOIDCDiscovery(oidcCfg.Issuer)
	if err != nil {
		Error(c, http.StatusBadGateway, "读取身份认证中心配置失败: "+err.Error())
		return
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", entry.RedirectURI)
	form.Set("client_id", oidcCfg.ClientID)
	form.Set("client_secret", oidcCfg.ClientSecret)
	form.Set("code_verifier", entry.CodeVerifier)

	tokenReq, err := http.NewRequest(http.MethodPost, discovery.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		Error(c, http.StatusInternalServerError, "创建令牌交换请求失败")
		return
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.Header.Set("Accept", "application/json")

	tokenResp, err := oidcHTTPClient.Do(tokenReq)
	if err != nil {
		Error(c, http.StatusBadGateway, "与身份认证中心交换令牌失败: "+err.Error())
		return
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		Error(c, http.StatusUnauthorized, fmt.Sprintf("授权码兑换令牌失败 (HTTP %d)", tokenResp.StatusCode))
		return
	}

	var tokenPayload struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(tokenResp.Body, 1<<20)).Decode(&tokenPayload); err != nil {
		Error(c, http.StatusBadGateway, "解析令牌响应失败")
		return
	}
	if strings.TrimSpace(tokenPayload.IDToken) == "" {
		Error(c, http.StatusUnauthorized, "身份认证响应缺少 id_token")
		return
	}

	now := oidcStates.now()
	claims, err := verifyAndParseIDToken(
		tokenPayload.IDToken,
		discovery.JWKSURI,
		oidcCfg.Issuer,
		oidcCfg.ClientID,
		entry.Nonce,
		now,
	)
	if err != nil {
		Error(c, http.StatusUnauthorized, "通行证身份令牌校验未通过: "+err.Error())
		return
	}

	if !oidcCfg.IsSubAllowed(claims.Sub) {
		Error(c, http.StatusForbidden, "当前通行证账号不在主理人允许白名单内")
		return
	}

	// 进入数据库写入阶段前持有恢复生命周期读锁，确保整库恢复期间数据文件不被替换
	if backup.IsRestoring() {
		Error(c, http.StatusServiceUnavailable, "系统正在从备份恢复，读写操作暂时不可用，请稍后重试")
		return
	}
	db.LockRead()
	defer db.UnlockRead()
	if backup.IsRestoring() {
		Error(c, http.StatusServiceUnavailable, "系统正在从备份恢复，读写操作暂时不可用，请稍后重试")
		return
	}

	if entry.Platform == "macos" || entry.Platform == "android" {
		hubURL := resolveHubURL(c)
		var pairTicket models.PairTicket

		txErr := db.DB.Transaction(func(tx *gorm.DB) error {
			if err := ensureSiteInitializedInTx(tx, now); err != nil {
				return err
			}
			var issueErr error
			pairTicket, issueErr = issuePairTicket(tx, hubURL, oidcPairTicketTTL)
			return issueErr
		})
		if txErr != nil {
			Error(c, http.StatusInternalServerError, "签发客户端一次性授权工单失败: "+txErr.Error())
			return
		}

		deepLink := buildPairDeepLink(hubURL, pairTicket.Ticket)
		renderOIDCNativeBridgePage(c, entry.Platform, hubURL, deepLink)
		return
	}

	// Web 展厅端：事务内确保站点已初始化并签发受信任 Web 设备记录，种入 30 天 HttpOnly Cookie
	device, rawDeviceToken := mintTrustedDevice("web", "Web 展厅 · Vesper Auth", entry.DeviceName, entry.DeviceModel, now)

	txErr := db.DB.Transaction(func(tx *gorm.DB) error {
		if err := ensureSiteInitializedInTx(tx, now); err != nil {
			return err
		}
		return tx.Create(&device).Error
	})
	if txErr != nil {
		Error(c, http.StatusInternalServerError, "签发主理人会话设备凭证失败: "+txErr.Error())
		return
	}

	middleware.SetSessionCookie(c, rawDeviceToken)

	target, ok := validateSafeReturnTo(entry.ReturnTo)
	if !ok {
		target = "/"
	}
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, target)
}

func ensureSiteInitializedInTx(tx *gorm.DB, now time.Time) error {
	var siteCfg models.SiteConfig
	if err := tx.Limit(1).Find(&siteCfg).Error; err != nil {
		return err
	}
	if siteCfg.ID == 0 {
		siteCfg = models.SiteConfig{
			SiteName:    "散页 · 个人生活志",
			Initialized: true,
		}
		return tx.Create(&siteCfg).Error
	}
	if !siteCfg.Initialized {
		return tx.Model(&models.SiteConfig{}).
			Where("id = ?", siteCfg.ID).
			Updates(map[string]any{
				"initialized": true,
				"updated_at":  now,
			}).Error
	}
	return nil
}

type oidcBridgeTemplateData struct {
	CSPNonce       string
	PlatformLabel  string
	HubURL         string
	DeepLinkURL    template.URL
	DeepLinkText   string
	ExpiresSeconds int
}

func renderOIDCNativeBridgePage(c *gin.Context, platform string, hubURL string, deepLink string) {
	nonceBytes := make([]byte, 16)
	_, _ = rand.Read(nonceBytes)
	cspNonce := base64.StdEncoding.EncodeToString(nonceBytes)

	platformLabel := "客户端"
	switch platform {
	case "macos":
		platformLabel = "macOS 桌面端"
	case "android":
		platformLabel = "Android 随行端"
	}

	csp := strings.Join([]string{
		"default-src 'none'",
		fmt.Sprintf("style-src 'nonce-%s'", cspNonce),
		fmt.Sprintf("script-src 'nonce-%s'", cspNonce),
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"form-action 'none'",
	}, "; ")

	c.Header("Content-Security-Policy", csp)
	c.Header("X-Frame-Options", "DENY")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")

	var buf bytes.Buffer
	err := oidcNativeBridgeTmpl.Execute(&buf, oidcBridgeTemplateData{
		CSPNonce:       cspNonce,
		PlatformLabel:  platformLabel,
		HubURL:         hubURL,
		DeepLinkURL:    template.URL(deepLink),
		DeepLinkText:   deepLink,
		ExpiresSeconds: int(oidcPairTicketTTL.Seconds()),
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, "渲染授权回跳页失败")
		return
	}

	c.Data(http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}
