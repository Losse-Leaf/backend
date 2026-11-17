package handlers

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
)

type mockIssuedCode struct {
	ClientID            string
	RedirectURI         string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	Sub                 string
}

type mockOIDCIdP struct {
	server *httptest.Server
	mu     sync.Mutex

	activeKey    *rsa.PrivateKey
	activeKid    string
	forgedKey    *rsa.PrivateKey
	clientID     string
	clientSecret string
	codes        map[string]mockIssuedCode
	jwksHits     int

	// 测试覆写钩子：用于模拟过期、伪造签名、篡改 iss / aud / nonce 或 alg=none
	nextIDTokenOverride func(issuer string, code mockIssuedCode) string
}

func newMockOIDCIdP(t *testing.T, clientID, clientSecret string) *mockOIDCIdP {
	t.Helper()

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	forgedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate forged rsa key: %v", err)
	}

	idp := &mockOIDCIdP{
		activeKey:    privKey,
		activeKid:    "vesper-key-1",
		forgedKey:    forgedKey,
		clientID:     clientID,
		clientSecret: clientSecret,
		codes:        make(map[string]mockIssuedCode),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := idp.server.URL
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"userinfo_endpoint":                     base + "/userinfo",
			"jwks_uri":                              base + "/jwks",
			"code_challenge_methods_supported":      []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		pub := &idp.activeKey.PublicKey
		kid := idp.activeKid
		idp.jwksHits++
		idp.mu.Unlock()

		nB64 := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
		eB64 := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"use": "sig",
					"alg": "RS256",
					"kid": kid,
					"n":   nB64,
					"e":   eB64,
				},
			},
		})
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if r.FormValue("grant_type") != "authorization_code" {
			http.Error(w, "unsupported grant_type", http.StatusBadRequest)
			return
		}
		if r.FormValue("client_id") != idp.clientID || r.FormValue("client_secret") != idp.clientSecret {
			http.Error(w, "invalid client credentials", http.StatusUnauthorized)
			return
		}

		codeVal := r.FormValue("code")
		idp.mu.Lock()
		issued, ok := idp.codes[codeVal]
		if ok {
			delete(idp.codes, codeVal)
		}
		overrideFn := idp.nextIDTokenOverride
		idp.nextIDTokenOverride = nil
		priv := idp.activeKey
		kid := idp.activeKid
		idp.mu.Unlock()

		if !ok {
			http.Error(w, "invalid or consumed code", http.StatusBadRequest)
			return
		}
		if r.FormValue("redirect_uri") != issued.RedirectURI {
			http.Error(w, "redirect_uri mismatch", http.StatusBadRequest)
			return
		}

		// 严格校验 PKCE S256
		verifier := r.FormValue("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		computedChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
		if issued.CodeChallengeMethod != "S256" || computedChallenge != issued.CodeChallenge {
			http.Error(w, "pkce verification failed", http.StatusBadRequest)
			return
		}

		var idToken string
		if overrideFn != nil {
			idToken = overrideFn(idp.server.URL, issued)
		} else {
			now := time.Now()
			idToken = signTestRS256JWT(t, priv, kid, map[string]any{
				"iss":   idp.server.URL,
				"sub":   issued.Sub,
				"aud":   issued.ClientID,
				"nonce": issued.Nonce,
				"iat":   now.Unix(),
				"exp":   now.Add(5 * time.Minute).Unix(),
				"email": "curator@starryheavens.uk",
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "vesper_at_" + generateRandomHex(8),
			"token_type":   "Bearer",
			"expires_in":   300,
			"id_token":     idToken,
		})
	})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(func() {
		idp.server.Close()
	})
	return idp
}

func (idp *mockOIDCIdP) issueAuthCode(authRedirectURL string, sub string) (code string, state string) {
	u, err := url.Parse(authRedirectURL)
	if err != nil {
		panic(err)
	}
	q := u.Query()
	code = "vesper_code_" + generateRandomHex(8)
	state = q.Get("state")

	idp.mu.Lock()
	idp.codes[code] = mockIssuedCode{
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		Nonce:               q.Get("nonce"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		Sub:                 sub,
	}
	idp.mu.Unlock()
	return code, state
}

func (idp *mockOIDCIdP) rotateSigningKey(t *testing.T, newKid string) {
	t.Helper()
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rotate rsa key: %v", err)
	}
	idp.mu.Lock()
	idp.activeKey = newKey
	idp.activeKid = newKid
	idp.mu.Unlock()
}

func signTestRS256JWT(t *testing.T, priv *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdrJSON, err := json.Marshal(map[string]string{
		"alg": "RS256",
		"typ": "JWT",
		"kid": kid,
	})
	if err != nil {
		t.Fatalf("marshal jwt header: %v", err)
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	hB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)
	pB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := hB64 + "." + pB64
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func setupOIDCTestRouter(t *testing.T, idp *mockOIDCIdP, allowedSubs []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	setupTestFullDB(t)
	resetOIDCCaches()
	oidcStates = newOIDCStateManager()

	prevCfg := config.AppConfig
	config.AppConfig = config.Config{
		ListenHost: "127.0.0.1",
		Port:       "8080",
		SiteURL:    "https://leaf.starryheavens.uk",
		OIDC: config.OIDCConfig{
			Enabled:      true,
			Issuer:       idp.server.URL,
			ClientID:     idp.clientID,
			ClientSecret: idp.clientSecret,
			AllowedSubs:  allowedSubs,
		},
	}
	t.Cleanup(func() {
		config.AppConfig = prevCfg
		resetOIDCCaches()
		oidcStates = newOIDCStateManager()
	})

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		v1.GET("/system/status", GetSystemStatus)
		v1.POST("/auth/claim", ClaimDevice)
		v1.GET("/auth/oidc/login", OIDCLogin)
		v1.GET("/auth/oidc/callback", OIDCCallback)
		v1.POST("/auth/pair/confirm", ConfirmPairing)
		v1.GET("/auth/session", GetSessionStatus)

		write := v1.Group("")
		write.Use(middleware.AdminAuthRequired())
		{
			write.POST("/transactions", CreateTransaction)
			write.GET("/transactions", GetTransactions)
			write.POST("/entries", CreateLifeEntry)
			write.GET("/auth/devices", GetTrustedDevices)
		}
	}
	return r
}

func extractCookie(resp *http.Response, name string) *http.Cookie {
	for _, ck := range resp.Cookies() {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

func TestOIDCWebBootstrapAndSessionLifecycle(t *testing.T) {
	idp := newMockOIDCIdP(t, "va_test_client_id", "va_test_secret_not_real")
	r := setupOIDCTestRouter(t, idp, []string{"usr_curator_001"})

	// 全新未初始化数据库
	db.DB.Create(&models.SiteConfig{Initialized: false, SiteName: "散页"})

	// 1. 探测 /api/v1/system/status 应返回 oidc_enabled=true 与 Vesper Auth
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	statusW := httptest.NewRecorder()
	r.ServeHTTP(statusW, statusReq)
	if statusW.Code != http.StatusOK {
		t.Fatalf("expected 200 on system/status, got %d", statusW.Code)
	}
	var statusBody struct {
		Data struct {
			Initialized      bool   `json:"initialized"`
			OIDCEnabled      bool   `json:"oidc_enabled"`
			OIDCProviderName string `json:"oidc_provider_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(statusW.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if statusBody.Data.Initialized {
		t.Fatal("expected initialized=false before first OIDC login")
	}
	if !statusBody.Data.OIDCEnabled || statusBody.Data.OIDCProviderName != "Vesper Auth" {
		t.Fatalf("unexpected OIDC status fields: %+v", statusBody.Data)
	}

	// 2. 发起 Web OIDC 登录（模拟 HTTPS 反代头 X-Forwarded-Proto: https）
	loginReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login?platform=web&return_to=%2F%23transactions", nil)
	loginReq.Header.Set("X-Forwarded-Proto", "https")
	loginW := httptest.NewRecorder()
	r.ServeHTTP(loginW, loginReq)

	if loginW.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect on /auth/oidc/login, got %d: %s", loginW.Code, loginW.Body.String())
	}
	authLoc := loginW.Header().Get("Location")
	if !strings.HasPrefix(authLoc, idp.server.URL+"/authorize?") {
		t.Fatalf("unexpected authorization redirect location: %s", authLoc)
	}
	authURL, _ := url.Parse(authLoc)
	if authURL.Query().Get("code_challenge_method") != "S256" || authURL.Query().Get("code_challenge") == "" {
		t.Fatalf("missing PKCE S256 parameters in redirect URL: %s", authLoc)
	}
	if authURL.Query().Get("redirect_uri") != "https://leaf.starryheavens.uk/api/v1/auth/oidc/callback" {
		t.Fatalf("expected SITE_URL redirect_uri, got %s", authURL.Query().Get("redirect_uri"))
	}

	bindingCookie := extractCookie(loginW.Result(), oidcBindingCookieName)
	if bindingCookie == nil || !bindingCookie.HttpOnly || !bindingCookie.Secure || bindingCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("expected Secure HttpOnly SameSite=Lax binding cookie, got %+v", bindingCookie)
	}

	// 3. 模拟 Vesper Auth 签发授权码并回调 /api/v1/auth/oidc/callback
	code, state := idp.issueAuthCode(authLoc, "usr_curator_001")
	cbReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	cbReq.Header.Set("X-Forwarded-Proto", "https")
	cbReq.AddCookie(bindingCookie)
	cbW := httptest.NewRecorder()
	r.ServeHTTP(cbW, cbReq)

	if cbW.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect on /auth/oidc/callback, got %d: %s", cbW.Code, cbW.Body.String())
	}
	if gotLoc := cbW.Header().Get("Location"); gotLoc != "/#transactions" {
		t.Fatalf("expected redirect back to /#transactions, got %q", gotLoc)
	}

	sessionCookie := extractCookie(cbW.Result(), middleware.SessionCookieName)
	if sessionCookie == nil || !strings.HasPrefix(sessionCookie.Value, "lldt_") || !sessionCookie.HttpOnly || !sessionCookie.Secure {
		t.Fatalf("expected Secure HttpOnly looseleaf_session cookie with lldt_ token, got %+v", sessionCookie)
	}

	// 4. 验证数据库事务已自动翻转 SiteConfig.Initialized = true 并落库受信任 Web 设备（仅存哈希）
	var siteCfg models.SiteConfig
	db.DB.First(&siteCfg)
	if !siteCfg.Initialized {
		t.Fatal("expected SiteConfig.Initialized = true after first OIDC login")
	}

	var devices []models.Device
	db.DB.Find(&devices)
	if len(devices) != 1 {
		t.Fatalf("expected 1 active device, got %d", len(devices))
	}
	if devices[0].Platform != "web" || devices[0].Status != "active" || devices[0].TokenHash != middleware.HashToken(sessionCookie.Value) {
		t.Fatalf("unexpected device record: %+v", devices[0])
	}

	// 5. 验证下发的 looseleaf_session Cookie 可无缝通过 /auth/session 与受保护写接口鉴权
	sessReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	sessReq.AddCookie(sessionCookie)
	sessW := httptest.NewRecorder()
	r.ServeHTTP(sessW, sessReq)
	if sessW.Code != http.StatusOK || !strings.Contains(sessW.Body.String(), `"is_admin":true`) {
		t.Fatalf("expected admin session status, got %d: %s", sessW.Code, sessW.Body.String())
	}

	txPayload, _ := json.Marshal(map[string]any{
		"amount":      3800,
		"type":        "expense",
		"category":    "餐饮",
		"title":       "手冲咖啡豆",
		"occurred_at": "2026-11-18T10:30:00+08:00",
	})
	txReq := httptest.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewReader(txPayload))
	txReq.Header.Set("Content-Type", "application/json")
	txReq.AddCookie(sessionCookie)
	txW := httptest.NewRecorder()
	r.ServeHTTP(txW, txReq)
	if txW.Code != http.StatusOK {
		t.Fatalf("expected 200 creating transaction via OIDC session cookie, got %d: %s", txW.Code, txW.Body.String())
	}
}

func TestOIDCNativeMacOSAndAndroidPairTicketFlow(t *testing.T) {
	idp := newMockOIDCIdP(t, "va_native_client", "va_native_secret_not_real")
	r := setupOIDCTestRouter(t, idp, []string{"usr_curator_001"})

	// 即使在全新未初始化库上直接从 macOS / Android 发起一键接入，也应自动完成激活与工单兑换
	db.DB.Create(&models.SiteConfig{Initialized: false, SiteName: "散页"})

	deepLinkRe := regexp.MustCompile(`looseleaf://pair\?[^"'<\s]+`)

	for _, tc := range []struct {
		platform    string
		deviceName  string
		deviceModel string
	}{
		{platform: "macos", deviceName: "Starry-MacBook-Pro", deviceModel: "Mac15,6"},
		{platform: "android", deviceName: "Pixel 9 Pro", deviceModel: "Pixel 9 Pro"},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			loginURL := "/api/v1/auth/oidc/login?platform=" + tc.platform +
				"&device_name=" + url.QueryEscape(tc.deviceName) +
				"&device_model=" + url.QueryEscape(tc.deviceModel)
			loginReq := httptest.NewRequest(http.MethodGet, loginURL, nil)
			loginW := httptest.NewRecorder()
			r.ServeHTTP(loginW, loginReq)
			if loginW.Code != http.StatusFound {
				t.Fatalf("expected 302 on login, got %d", loginW.Code)
			}

			bindingCookie := extractCookie(loginW.Result(), oidcBindingCookieName)
			code, state := idp.issueAuthCode(loginW.Header().Get("Location"), "usr_curator_001")

			cbReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state="+state, nil)
			cbReq.AddCookie(bindingCookie)
			cbW := httptest.NewRecorder()
			r.ServeHTTP(cbW, cbReq)

			if cbW.Code != http.StatusOK {
				t.Fatalf("expected 200 HTML bridge page, got %d: %s", cbW.Code, cbW.Body.String())
			}
			csp := cbW.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'nonce-") || !strings.Contains(csp, "frame-ancestors 'none'") {
				t.Fatalf("expected strict nonce-based CSP header, got %q", csp)
			}

			htmlBody := cbW.Body.String()
			if strings.Contains(htmlBody, "#ZgotmplZ") {
				t.Fatalf("bridge HTML must not contain sanitized template error #ZgotmplZ: %s", htmlBody)
			}
			if !strings.Contains(htmlBody, "打开《散页》完成授权") || !strings.Contains(htmlBody, "复制一次性授权深链") {
				t.Fatalf("bridge HTML missing expected editorial fallback buttons")
			}

			rawDeepLink := deepLinkRe.FindString(htmlBody)
			if rawDeepLink == "" {
				t.Fatalf("failed to find looseleaf://pair deep link in HTML: %s", htmlBody)
			}
			// HTML 属性中的 &amp; 还原为 &
			rawDeepLink = strings.ReplaceAll(rawDeepLink, "&amp;", "&")
			dlURL, err := url.Parse(rawDeepLink)
			if err != nil {
				t.Fatalf("parse deep link %q: %v", rawDeepLink, err)
			}
			hubParam := dlURL.Query().Get("hub")
			ticketParam := dlURL.Query().Get("ticket")
			if hubParam != "https://leaf.starryheavens.uk" || !strings.HasPrefix(ticketParam, "pair_tk_") {
				t.Fatalf("unexpected deep link params: hub=%q ticket=%q", hubParam, ticketParam)
			}

			// 校验工单有效期为 90 秒
			var ticketRec models.PairTicket
			if err := db.DB.Where("ticket = ?", ticketParam).First(&ticketRec).Error; err != nil {
				t.Fatalf("pair ticket not persisted: %v", err)
			}
			ttl := time.Until(ticketRec.ExpiresAt)
			if ttl <= 0 || ttl > 95*time.Second {
				t.Fatalf("expected ~90s TTL for OIDC pair ticket, got %v", ttl)
			}

			// 模拟原生客户端捕获深链后调用 POST /api/v1/auth/pair/confirm 换取独立 Bearer Token
			confirmBody, _ := json.Marshal(ConfirmPairingRequest{
				Ticket:      ticketParam,
				DeviceName:  tc.deviceName,
				Platform:    tc.platform,
				DeviceModel: tc.deviceModel,
			})
			confirmReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(confirmBody))
			confirmReq.Header.Set("Content-Type", "application/json")
			confirmW := httptest.NewRecorder()
			r.ServeHTTP(confirmW, confirmReq)
			if confirmW.Code != http.StatusOK {
				t.Fatalf("expected 200 confirming pair ticket, got %d: %s", confirmW.Code, confirmW.Body.String())
			}

			var confirmRes struct {
				Data struct {
					DeviceID    string `json:"device_id"`
					DeviceToken string `json:"device_token"`
					Platform    string `json:"platform"`
					HubURL      string `json:"hub_url"`
				} `json:"data"`
			}
			_ = json.Unmarshal(confirmW.Body.Bytes(), &confirmRes)
			if !strings.HasPrefix(confirmRes.Data.DeviceToken, "lldt_") || confirmRes.Data.Platform != tc.platform {
				t.Fatalf("unexpected confirm response: %+v", confirmRes.Data)
			}

			// 使用签发的 Bearer Token 访问受保护接口
			devReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/devices", nil)
			devReq.Header.Set("Authorization", "Bearer "+confirmRes.Data.DeviceToken)
			devW := httptest.NewRecorder()
			r.ServeHTTP(devW, devReq)
			if devW.Code != http.StatusOK {
				t.Fatalf("expected 200 with issued Bearer token, got %d: %s", devW.Code, devW.Body.String())
			}

			// 二次重放同一工单必须被拒绝
			replayReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pair/confirm", bytes.NewReader(confirmBody))
			replayReq.Header.Set("Content-Type", "application/json")
			replayW := httptest.NewRecorder()
			r.ServeHTTP(replayW, replayReq)
			if replayW.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 on ticket replay, got %d", replayW.Code)
			}
		})
	}
}

func TestOIDCSecurityGuardsAndJWKSKeyRotation(t *testing.T) {
	idp := newMockOIDCIdP(t, "va_sec_client", "va_sec_secret_not_real")
	r := setupOIDCTestRouter(t, idp, []string{"usr_curator_only"})
	db.DB.Create(&models.SiteConfig{Initialized: false, SiteName: "散页"})

	t.Run("1. Blocks open redirect return_to values", func(t *testing.T) {
		malicious := []string{
			"https://evil.example.com/phish",
			"http://evil.example.com",
			"//evil.example.com/path",
			"///evil.example.com",
			`/\\evil.example.com`,
			"javascript:alert(1)",
			"data:text/html,<script>alert(1)</script>",
			"relative/without/leading/slash",
			"/valid\r\nLocation: https://evil.example.com",
		}
		for _, target := range malicious {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login?return_to="+url.QueryEscape(target), nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for malicious return_to %q, got %d", target, w.Code)
			}
		}
	})

	t.Run("2. Rejects tampered state, missing binding cookie (Login CSRF), and state replay", func(t *testing.T) {
		loginReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login?platform=web", nil)
		loginW := httptest.NewRecorder()
		r.ServeHTTP(loginW, loginReq)
		bindingCookie := extractCookie(loginW.Result(), oidcBindingCookieName)
		code, state := idp.issueAuthCode(loginW.Header().Get("Location"), "usr_curator_only")

		// 2a. 篡改 state 参数
		badStateReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state=tampered_"+state, nil)
		badStateReq.AddCookie(bindingCookie)
		badStateW := httptest.NewRecorder()
		r.ServeHTTP(badStateW, badStateReq)
		if badStateW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for tampered state, got %d", badStateW.Code)
		}

		// 2b. 缺失绑定 Cookie（模拟跨站 Login CSRF）
		csrfReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state="+state, nil)
		csrfW := httptest.NewRecorder()
		r.ServeHTTP(csrfW, csrfReq)
		if csrfW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing binding cookie, got %d", csrfW.Code)
		}

		// 2c. 携带真实绑定 Cookie 正常消费一次后，重放同一 state 必须失败
		okReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state="+state, nil)
		okReq.AddCookie(bindingCookie)
		okW := httptest.NewRecorder()
		r.ServeHTTP(okW, okReq)
		if okW.Code != http.StatusFound {
			t.Fatalf("expected 302 on valid state + cookie, got %d: %s", okW.Code, okW.Body.String())
		}

		replayReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state="+state, nil)
		replayReq.AddCookie(bindingCookie)
		replayW := httptest.NewRecorder()
		r.ServeHTTP(replayW, replayReq)
		if replayW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on replayed state, got %d", replayW.Code)
		}
	})

	t.Run("3. Rejects non-whitelisted sub with 403 Forbidden without initializing or issuing device", func(t *testing.T) {
		// 重置为未初始化态以确认非白名单账号绝不会触发创刊激活
		db.DB.Model(&models.SiteConfig{}).Where("1 = 1").Update("initialized", false)
		var beforeDevices int64
		db.DB.Model(&models.Device{}).Count(&beforeDevices)

		loginReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login?platform=web", nil)
		loginW := httptest.NewRecorder()
		r.ServeHTTP(loginW, loginReq)
		bindingCookie := extractCookie(loginW.Result(), oidcBindingCookieName)
		code, state := idp.issueAuthCode(loginW.Header().Get("Location"), "usr_stranger_999")

		cbReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state="+state, nil)
		cbReq.AddCookie(bindingCookie)
		cbW := httptest.NewRecorder()
		r.ServeHTTP(cbW, cbReq)

		if cbW.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-whitelisted sub, got %d: %s", cbW.Code, cbW.Body.String())
		}
		if extractCookie(cbW.Result(), middleware.SessionCookieName) != nil {
			t.Fatal("must not issue looseleaf_session cookie for rejected sub")
		}
		var cfg models.SiteConfig
		db.DB.First(&cfg)
		if cfg.Initialized {
			t.Fatal("non-whitelisted sub must not flip SiteConfig.Initialized to true")
		}
		var afterDevices int64
		db.DB.Model(&models.Device{}).Count(&afterDevices)
		if afterDevices != beforeDevices {
			t.Fatalf("expected no new device created, before=%d after=%d", beforeDevices, afterDevices)
		}
	})

	t.Run("4. Rejects expired ID Token and forged RSA signature", func(t *testing.T) {
		// 4a. 过期 ID Token
		idp.mu.Lock()
		idp.nextIDTokenOverride = func(issuer string, code mockIssuedCode) string {
			return signTestRS256JWT(t, idp.activeKey, idp.activeKid, map[string]any{
				"iss":   issuer,
				"sub":   "usr_curator_only",
				"aud":   code.ClientID,
				"nonce": code.Nonce,
				"iat":   time.Now().Add(-10 * time.Minute).Unix(),
				"exp":   time.Now().Add(-1 * time.Minute).Unix(),
			})
		}
		idp.mu.Unlock()

		loginReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login?platform=web", nil)
		loginW := httptest.NewRecorder()
		r.ServeHTTP(loginW, loginReq)
		ck := extractCookie(loginW.Result(), oidcBindingCookieName)
		code, state := idp.issueAuthCode(loginW.Header().Get("Location"), "usr_curator_only")

		cbReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state="+state, nil)
		cbReq.AddCookie(ck)
		cbW := httptest.NewRecorder()
		r.ServeHTTP(cbW, cbReq)
		if cbW.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for expired id_token, got %d: %s", cbW.Code, cbW.Body.String())
		}

		// 4b. 伪造私钥签名的 ID Token
		idp.mu.Lock()
		idp.nextIDTokenOverride = func(issuer string, code mockIssuedCode) string {
			return signTestRS256JWT(t, idp.forgedKey, idp.activeKid, map[string]any{
				"iss":   issuer,
				"sub":   "usr_curator_only",
				"aud":   code.ClientID,
				"nonce": code.Nonce,
				"iat":   time.Now().Unix(),
				"exp":   time.Now().Add(5 * time.Minute).Unix(),
			})
		}
		idp.mu.Unlock()

		loginReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login?platform=web", nil)
		loginW2 := httptest.NewRecorder()
		r.ServeHTTP(loginW2, loginReq2)
		ck2 := extractCookie(loginW2.Result(), oidcBindingCookieName)
		code2, state2 := idp.issueAuthCode(loginW2.Header().Get("Location"), "usr_curator_only")

		cbReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code2+"&state="+state2, nil)
		cbReq2.AddCookie(ck2)
		cbW2 := httptest.NewRecorder()
		r.ServeHTTP(cbW2, cbReq2)
		if cbW2.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for forged id_token signature, got %d: %s", cbW2.Code, cbW2.Body.String())
		}
	})

	t.Run("5. Automatically refreshes JWKS once when IdP rotates signing kid", func(t *testing.T) {
		idp.rotateSigningKey(t, "vesper-key-rotated-2")

		loginReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login?platform=web", nil)
		loginW := httptest.NewRecorder()
		r.ServeHTTP(loginW, loginReq)
		ck := extractCookie(loginW.Result(), oidcBindingCookieName)
		code, state := idp.issueAuthCode(loginW.Header().Get("Location"), "usr_curator_only")

		cbReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state="+state, nil)
		cbReq.AddCookie(ck)
		cbW := httptest.NewRecorder()
		r.ServeHTTP(cbW, cbReq)
		if cbW.Code != http.StatusFound {
			t.Fatalf("expected 302 after automatic JWKS refresh on rotated kid, got %d: %s", cbW.Code, cbW.Body.String())
		}
	})
}

func TestInternalBridgeTokenLoopbackBoundary(t *testing.T) {
	idp := newMockOIDCIdP(t, "va_bridge_test", "va_bridge_secret_not_real")
	r := setupOIDCTestRouter(t, idp, []string{"usr_curator_001"})
	db.DB.Create(&models.SiteConfig{Initialized: true, SiteName: "散页"})

	const testBridgeToken = "ll_bridge_test_token_random_val"
	config.AppConfig.InternalBridgeToken = testBridgeToken

	postTx := func(remoteAddr string, bridgeHeader string, extraHeaders map[string]string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]any{
			"amount":      2500,
			"type":        "expense",
			"category":    "餐饮",
			"title":       "BotHub 自动记账",
			"occurred_at": "2026-11-18T12:00:00+08:00",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewReader(payload))
		req.RemoteAddr = remoteAddr
		req.Header.Set("Content-Type", "application/json")
		if bridgeHeader != "" {
			req.Header.Set("X-Bridge-Token", bridgeHeader)
		}
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 1. 纯本机 IPv4 127.0.0.1 回环 + 正确 X-Bridge-Token + 无反代头 -> 放行
	if w := postTx("127.0.0.1:54321", testBridgeToken, nil); w.Code != http.StatusOK {
		t.Fatalf("expected 200 for pure IPv4 loopback bridge request, got %d: %s", w.Code, w.Body.String())
	}

	// 2. 纯本机 IPv6 [::1] 回环 + 正确 X-Bridge-Token + 无反代头 -> 放行
	if w := postTx("[::1]:54322", testBridgeToken, nil); w.Code != http.StatusOK {
		t.Fatalf("expected 200 for pure IPv6 loopback bridge request, got %d: %s", w.Code, w.Body.String())
	}

	// 3. 携带外部反代头（模拟本机 Nginx / Cloudflare 转发的公网流量）必须严格拒绝
	for _, hdr := range []map[string]string{
		{"X-Forwarded-For": "198.51.100.42"},
		{"CF-Connecting-IP": "198.51.100.42"},
		{"X-Real-IP": "198.51.100.42"},
		{"Forwarded": "for=198.51.100.42;proto=https"},
	} {
		if w := postTx("127.0.0.1:54321", testBridgeToken, hdr); w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 when proxy header %v is present, got %d", hdr, w.Code)
		}
	}

	// 4. 非回环底层 TCP RemoteAddr 即使携带正确 X-Bridge-Token 也必须拒绝
	if w := postTx("192.168.1.20:54321", testBridgeToken, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for non-loopback RemoteAddr, got %d", w.Code)
	}
	if w := postTx("203.0.113.9:54321", testBridgeToken, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for public RemoteAddr, got %d", w.Code)
	}

	// 5. 错误或缺失 X-Bridge-Token 必须拒绝
	if w := postTx("127.0.0.1:54321", "wrong_bridge_token", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong X-Bridge-Token, got %d", w.Code)
	}
	if w := postTx("127.0.0.1:54321", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing X-Bridge-Token, got %d", w.Code)
	}
}
