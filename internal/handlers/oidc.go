package handlers

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"loose-leaf/internal/backup"
	"loose-leaf/internal/config"
	"loose-leaf/internal/db"
	"loose-leaf/internal/middleware"
	"loose-leaf/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	oidcBindingCookieName = "looseleaf_oidc_binding"
	oidcStateTTL          = 10 * time.Minute
	oidcMaxPendingStates  = 1024
	oidcMetadataCacheTTL  = 15 * time.Minute
	oidcPairTicketTTL     = 90 * time.Second
)

// oidcStateEntry 保存单次 OAuth 2.0 / OIDC + PKCE 登录会话的短期绑定上下文。
type oidcStateEntry struct {
	State         string
	CookieBinding string
	Nonce         string
	CodeVerifier  string
	RedirectURI   string
	Platform      string // "web" | "macos" | "android"
	DeviceName    string
	DeviceModel   string
	ReturnTo      string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type oidcStateManager struct {
	mu    sync.Mutex
	items map[string]oidcStateEntry
	nowFn func() time.Time
}

func newOIDCStateManager() *oidcStateManager {
	return &oidcStateManager{
		items: make(map[string]oidcStateEntry),
		nowFn: time.Now,
	}
}

func (m *oidcStateManager) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

func (m *oidcStateManager) sweepExpiredLocked(now time.Time) {
	for k, v := range m.items {
		if now.After(v.ExpiresAt) {
			delete(m.items, k)
		}
	}
}

func (m *oidcStateManager) Put(entry oidcStateEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	m.sweepExpiredLocked(now)

	// 容量上限保护：达到上限时淘汰最早创建的待决状态，防止内存无界增长
	for len(m.items) >= oidcMaxPendingStates {
		var oldestKey string
		var oldestAt time.Time
		first := true
		for k, v := range m.items {
			if first || v.CreatedAt.Before(oldestAt) {
				oldestKey = k
				oldestAt = v.CreatedAt
				first = false
			}
		}
		if oldestKey == "" {
			break
		}
		delete(m.items, oldestKey)
	}

	m.items[entry.State] = entry
}

var (
	errOIDCStateInvalid = errors.New("登录状态已过期或校验不匹配，请重新发起登录")
)

// Consume 原子核验 state 与短期绑定 Cookie，并在校验通过后立即一次性销毁。
func (m *oidcStateManager) Consume(state, cookieBinding string) (oidcStateEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	m.sweepExpiredLocked(now)

	if state == "" || cookieBinding == "" {
		return oidcStateEntry{}, errOIDCStateInvalid
	}

	entry, ok := m.items[state]
	if !ok {
		return oidcStateEntry{}, errOIDCStateInvalid
	}

	if subtle.ConstantTimeCompare([]byte(cookieBinding), []byte(entry.CookieBinding)) != 1 {
		return oidcStateEntry{}, errOIDCStateInvalid
	}

	delete(m.items, state)
	return entry, nil
}

type oidcDiscoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

type cachedDiscovery struct {
	doc       oidcDiscoveryDocument
	fetchedAt time.Time
}

type jwkItem struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type jwksResponse struct {
	Keys []jwkItem `json:"keys"`
}

type cachedJWKS struct {
	keys      []jwkItem
	fetchedAt time.Time
}

var (
	oidcStates      = newOIDCStateManager()
	oidcHTTPClient  = &http.Client{Timeout: 10 * time.Second}
	oidcCacheMu     sync.RWMutex
	discoveryCache  = make(map[string]cachedDiscovery)
	jwksCacheByURI  = make(map[string]cachedJWKS)
)

func resetOIDCCaches() {
	oidcCacheMu.Lock()
	discoveryCache = make(map[string]cachedDiscovery)
	jwksCacheByURI = make(map[string]cachedJWKS)
	oidcCacheMu.Unlock()
}

func generateRandomBase64URL(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func computePKCES256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// validateSafeReturnTo 严格校验站内相对路径，拦截外部 URL、协议相对路径 //、反斜杠与控制字符，防御开放重定向。
func validateSafeReturnTo(raw string) (string, bool) {
	if raw == "" {
		return "/", true
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil {
		return "", false
	}
	if !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		return "", false
	}
	return raw, true
}

func sanitizeDeviceLabel(raw string, maxRunes int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(cleaned) <= maxRunes {
		return cleaned
	}
	runes := []rune(cleaned)
	return strings.TrimSpace(string(runes[:maxRunes]))
}

func resolveOIDCRedirectURI(c *gin.Context, oidcCfg config.OIDCConfig) string {
	if explicit := strings.TrimSpace(oidcCfg.RedirectURI); explicit != "" {
		return explicit
	}
	return resolveHubURL(c) + "/api/v1/auth/oidc/callback"
}

func setOIDCBindingCookie(c *gin.Context, value string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     oidcBindingCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(oidcStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   middleware.IsSecureRequest(c),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearOIDCBindingCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     oidcBindingCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   middleware.IsSecureRequest(c),
		SameSite: http.SameSiteLaxMode,
	})
}

func fetchOIDCDiscovery(issuer string) (oidcDiscoveryDocument, error) {
	normalizedIssuer := strings.TrimRight(strings.TrimSpace(issuer), "/")
	if normalizedIssuer == "" {
		return oidcDiscoveryDocument{}, errors.New("OIDC_ISSUER 未配置")
	}

	oidcCacheMu.RLock()
	if cached, ok := discoveryCache[normalizedIssuer]; ok && time.Since(cached.fetchedAt) < oidcMetadataCacheTTL {
		oidcCacheMu.RUnlock()
		return cached.doc, nil
	}
	oidcCacheMu.RUnlock()

	wellKnownURL := normalizedIssuer + "/.well-known/openid-configuration"
	req, err := http.NewRequest(http.MethodGet, wellKnownURL, nil)
	if err != nil {
		return oidcDiscoveryDocument{}, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := oidcHTTPClient.Do(req)
	if err != nil {
		return oidcDiscoveryDocument{}, fmt.Errorf("请求 OIDC Discovery 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return oidcDiscoveryDocument{}, fmt.Errorf("OIDC Discovery 返回 HTTP %d", resp.StatusCode)
	}

	var doc oidcDiscoveryDocument
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return oidcDiscoveryDocument{}, fmt.Errorf("解析 OIDC Discovery 失败: %w", err)
	}

	if strings.TrimSpace(doc.AuthorizationEndpoint) == "" ||
		strings.TrimSpace(doc.TokenEndpoint) == "" ||
		strings.TrimSpace(doc.JWKSURI) == "" {
		return oidcDiscoveryDocument{}, errors.New("OIDC Discovery 缺少必需端点")
	}
	if strings.TrimRight(strings.TrimSpace(doc.Issuer), "/") != normalizedIssuer {
		return oidcDiscoveryDocument{}, errors.New("OIDC Discovery issuer 与配置不一致")
	}

	oidcCacheMu.Lock()
	discoveryCache[normalizedIssuer] = cachedDiscovery{
		doc:       doc,
		fetchedAt: time.Now(),
	}
	oidcCacheMu.Unlock()

	return doc, nil
}

func fetchJWKS(jwksURI string, forceRefresh bool) ([]jwkItem, bool, error) {
	if !forceRefresh {
		oidcCacheMu.RLock()
		if cached, ok := jwksCacheByURI[jwksURI]; ok && time.Since(cached.fetchedAt) < oidcMetadataCacheTTL {
			keys := make([]jwkItem, len(cached.keys))
			copy(keys, cached.keys)
			oidcCacheMu.RUnlock()
			return keys, true, nil
		}
		oidcCacheMu.RUnlock()
	}

	req, err := http.NewRequest(http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := oidcHTTPClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("拉取 JWKS 公钥集失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("JWKS 端点返回 HTTP %d", resp.StatusCode)
	}

	var parsed jwksResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, false, fmt.Errorf("解析 JWKS 响应失败: %w", err)
	}
	if len(parsed.Keys) == 0 {
		return nil, false, errors.New("JWKS 公钥集为空")
	}

	oidcCacheMu.Lock()
	jwksCacheByURI[jwksURI] = cachedJWKS{
		keys:      parsed.Keys,
		fetchedAt: time.Now(),
	}
	oidcCacheMu.Unlock()

	return parsed.Keys, false, nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type audienceList []string

func (a *audienceList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*a = nil
		return nil
	}
	if trimmed[0] == '"' {
		var single string
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return err
		}
		*a = []string{single}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(trimmed, &multi); err != nil {
		return err
	}
	*a = multi
	return nil
}

type oidcIDTokenClaims struct {
	Iss   string       `json:"iss"`
	Sub   string       `json:"sub"`
	Aud   audienceList `json:"aud"`
	Azp   string       `json:"azp"`
	Exp   int64        `json:"exp"`
	Iat   int64        `json:"iat"`
	Nbf   int64        `json:"nbf"`
	Nonce string       `json:"nonce"`
	Email string       `json:"email"`
	Name  string       `json:"name"`
}

func selectJWK(keys []jwkItem, kid string, expectedKty string) (jwkItem, bool) {
	for _, k := range keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if k.Kty != expectedKty {
			continue
		}
		if kid != "" {
			if k.Kid == kid {
				return k, true
			}
		} else {
			return k, true
		}
	}
	return jwkItem{}, false
}

func parseRSAPublicKey(k jwkItem) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil || len(nBytes) == 0 {
		return nil, errors.New("无效的 RSA 模数 n")
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil || len(eBytes) == 0 {
		return nil, errors.New("无效的 RSA 指数 e")
	}
	eBig := new(big.Int).SetBytes(eBytes)
	if !eBig.IsInt64() || eBig.Int64() <= 1 {
		return nil, errors.New("非法的 RSA 指数 e")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(eBig.Int64()),
	}, nil
}

func parseECDSAPublicKey(k jwkItem) (*ecdsa.PublicKey, int, error) {
	var curve elliptic.Curve
	var coordSize int
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
		coordSize = 32
	case "P-384":
		curve = elliptic.P384()
		coordSize = 48
	case "P-521":
		curve = elliptic.P521()
		coordSize = 66
	default:
		return nil, 0, fmt.Errorf("不支持的椭圆曲线: %s", k.Crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil || len(xBytes) == 0 {
		return nil, 0, errors.New("无效的 EC 公钥坐标 x")
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil || len(yBytes) == 0 {
		return nil, 0, errors.New("无效的 EC 公钥坐标 y")
	}
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	if !curve.IsOnCurve(x, y) {
		return nil, 0, errors.New("EC 公钥坐标不在目标曲线上")
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, coordSize, nil
}

func verifyJWSSignature(alg string, signingInput []byte, sig []byte, key jwkItem) error {
	switch alg {
	case "RS256", "RS384", "RS512":
		pub, err := parseRSAPublicKey(key)
		if err != nil {
			return err
		}
		var hashAlg crypto.Hash
		var digest []byte
		switch alg {
		case "RS256":
			hashAlg = crypto.SHA256
			sum := sha256.Sum256(signingInput)
			digest = sum[:]
		case "RS384":
			hashAlg = crypto.SHA384
			sum := sha512.Sum384(signingInput)
			digest = sum[:]
		case "RS512":
			hashAlg = crypto.SHA512
			sum := sha512.Sum512(signingInput)
			digest = sum[:]
		}
		return rsa.VerifyPKCS1v15(pub, hashAlg, digest, sig)

	case "ES256", "ES384", "ES512":
		pub, coordSize, err := parseECDSAPublicKey(key)
		if err != nil {
			return err
		}
		if len(sig) != coordSize*2 {
			return errors.New("ECDSA JWS 签名长度不合法")
		}
		var digest []byte
		switch alg {
		case "ES256":
			if key.Crv != "P-256" {
				return errors.New("ES256 要求 P-256 曲线")
			}
			sum := sha256.Sum256(signingInput)
			digest = sum[:]
		case "ES384":
			if key.Crv != "P-384" {
				return errors.New("ES384 要求 P-384 曲线")
			}
			sum := sha512.Sum384(signingInput)
			digest = sum[:]
		case "ES512":
			if key.Crv != "P-521" {
				return errors.New("ES512 要求 P-521 曲线")
			}
			sum := sha512.Sum512(signingInput)
			digest = sum[:]
		}
		r := new(big.Int).SetBytes(sig[:coordSize])
		s := new(big.Int).SetBytes(sig[coordSize:])
		if !ecdsa.Verify(pub, digest, r, s) {
			return errors.New("ECDSA 签名校验未通过")
		}
		return nil

	case "EdDSA":
		if key.Crv != "Ed25519" {
			return errors.New("EdDSA 仅支持 Ed25519 曲线")
		}
		xBytes, err := base64.RawURLEncoding.DecodeString(key.X)
		if err != nil || len(xBytes) != ed25519.PublicKeySize {
			return errors.New("无效的 Ed25519 公钥")
		}
		if !ed25519.Verify(ed25519.PublicKey(xBytes), signingInput, sig) {
			return errors.New("Ed25519 签名校验未通过")
		}
		return nil

	default:
		return fmt.Errorf("不支持的签名算法: %s", alg)
	}
}

func expectedKtyForAlg(alg string) (string, error) {
	switch alg {
	case "RS256", "RS384", "RS512":
		return "RSA", nil
	case "ES256", "ES384", "ES512":
		return "EC", nil
	case "EdDSA":
		return "OKP", nil
	default:
		return "", fmt.Errorf("拒绝不安全或不支持的 JWS 算法: %s", alg)
	}
}

func verifyAndParseIDToken(
	rawToken string,
	jwksURI string,
	expectedIssuer string,
	expectedClientID string,
	expectedNonce string,
	now time.Time,
) (oidcIDTokenClaims, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return oidcIDTokenClaims{}, errors.New("ID Token 格式不合法")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return oidcIDTokenClaims{}, errors.New("解析 ID Token 头部失败")
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return oidcIDTokenClaims{}, errors.New("反序列化 ID Token 头部失败")
	}

	expectedKty, err := expectedKtyForAlg(hdr.Alg)
	if err != nil {
		return oidcIDTokenClaims{}, err
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sigBytes) == 0 {
		return oidcIDTokenClaims{}, errors.New("解析 ID Token 签名失败")
	}

	keys, fromCache, err := fetchJWKS(jwksURI, false)
	if err != nil {
		return oidcIDTokenClaims{}, err
	}
	chosenKey, found := selectJWK(keys, hdr.Kid, expectedKty)
	// 若命中缓存但未找到对应 kid（密钥轮换场景），强制刷新一次 JWKS 再匹配
	if !found && fromCache {
		keys, _, err = fetchJWKS(jwksURI, true)
		if err != nil {
			return oidcIDTokenClaims{}, err
		}
		chosenKey, found = selectJWK(keys, hdr.Kid, expectedKty)
	}
	if !found {
		return oidcIDTokenClaims{}, errors.New("未找到匹配的 JWKS 签名公钥")
	}

	signingInput := []byte(parts[0] + "." + parts[1])
	if err := verifyJWSSignature(hdr.Alg, signingInput, sigBytes, chosenKey); err != nil {
		return oidcIDTokenClaims{}, fmt.Errorf("ID Token 签名校验失败: %w", err)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return oidcIDTokenClaims{}, errors.New("解析 ID Token 载荷失败")
	}
	var claims oidcIDTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return oidcIDTokenClaims{}, errors.New("反序列化 ID Token 载荷失败")
	}

	if strings.TrimRight(strings.TrimSpace(claims.Iss), "/") != strings.TrimRight(strings.TrimSpace(expectedIssuer), "/") {
		return oidcIDTokenClaims{}, errors.New("ID Token iss 声明不匹配")
	}

	audMatched := false
	for _, aud := range claims.Aud {
		if subtle.ConstantTimeCompare([]byte(aud), []byte(expectedClientID)) == 1 {
			audMatched = true
		}
	}
	if !audMatched {
		return oidcIDTokenClaims{}, errors.New("ID Token aud 声明不匹配")
	}
	if len(claims.Aud) > 1 && claims.Azp != "" && subtle.ConstantTimeCompare([]byte(claims.Azp), []byte(expectedClientID)) != 1 {
		return oidcIDTokenClaims{}, errors.New("ID Token azp 声明不匹配")
	}

	nowUnix := now.Unix()
	if claims.Exp <= 0 || nowUnix >= claims.Exp {
		return oidcIDTokenClaims{}, errors.New("ID Token 已过期")
	}
	if claims.Nbf > 0 && nowUnix+60 < claims.Nbf {
		return oidcIDTokenClaims{}, errors.New("ID Token 尚未生效")
	}

	if expectedNonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(expectedNonce)) != 1 {
		return oidcIDTokenClaims{}, errors.New("ID Token nonce 声明不匹配")
	}

	if strings.TrimSpace(claims.Sub) == "" {
		return oidcIDTokenClaims{}, errors.New("ID Token 缺少 sub 声明")
	}

	return claims, nil
}

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
		ticketStr := fmt.Sprintf("pair_tk_%s", generateRandomHex(12))
		expiresAt := now.Add(oidcPairTicketTTL)

		txErr := db.DB.Transaction(func(tx *gorm.DB) error {
			if err := ensureSiteInitializedInTx(tx, now); err != nil {
				return err
			}
			pairTicket := models.PairTicket{
				Ticket:    ticketStr,
				PinCode:   "",
				HubURL:    hubURL,
				ExpiresAt: expiresAt,
				Used:      false,
			}
			return tx.Create(&pairTicket).Error
		})
		if txErr != nil {
			Error(c, http.StatusInternalServerError, "签发客户端一次性授权工单失败: "+txErr.Error())
			return
		}

		q := url.Values{}
		q.Set("hub", hubURL)
		q.Set("ticket", ticketStr)
		deepLink := "looseleaf://pair?" + q.Encode()

		renderOIDCNativeBridgePage(c, entry.Platform, hubURL, deepLink)
		return
	}

	// Web 展厅端：事务内确保站点已初始化并签发受信任 Web 设备记录，种入 30 天 HttpOnly Cookie
	deviceName := entry.DeviceName
	if deviceName == "" {
		deviceName = "Web 展厅 · Vesper Auth"
	}
	rawDeviceToken := fmt.Sprintf("lldt_%s", generateRandomHex(16))
	device := models.Device{
		DeviceID:     fmt.Sprintf("dev_web_%s_%s", now.Format("20060102"), generateRandomHex(4)),
		Name:         deviceName,
		Platform:     "web",
		DeviceModel:  entry.DeviceModel,
		TokenHash:    middleware.HashToken(rawDeviceToken),
		Status:       "active",
		PairedAt:     now,
		LastActiveAt: now,
	}

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

var oidcNativeBridgeTmpl = template.Must(template.New("oidc_native_bridge").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="referrer" content="no-referrer">
  <title>散页 · 通行证授权回跳</title>
  <style nonce="{{.CSPNonce}}">
    :root {
      --bg-archive: #f9f8f5;
      --bg-subtle: #f2f0eb;
      --ink-primary: #181716;
      --ink-secondary: #57544e;
      --ink-muted: #8c8880;
      --border-subtle: rgba(24, 23, 22, 0.10);
      --border-divider: rgba(24, 23, 22, 0.16);
      --accent-warm: #a24f2b;
      --accent-moss: #3f5a4a;
      --btn-ink-fg: #f9f8f5;
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg-archive: #141312;
        --bg-subtle: #1d1b19;
        --ink-primary: #ede8e1;
        --ink-secondary: #a8a399;
        --ink-muted: #6e6a62;
        --border-subtle: rgba(237, 232, 225, 0.10);
        --border-divider: rgba(237, 232, 225, 0.16);
        --accent-warm: #c85635;
        --accent-moss: #507661;
        --btn-ink-fg: #141312;
      }
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      min-height: 100dvh;
      background-color: var(--bg-archive);
      color: var(--ink-primary);
      font-family: "Newsreader", "Songti SC", "Source Han Serif SC", Georgia, serif;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 24px 16px;
      -webkit-font-smoothing: antialiased;
    }
    .sheet {
      width: 100%;
      max-width: 460px;
      background-color: var(--bg-subtle);
      border: 1px solid var(--border-subtle);
      padding: 32px 28px;
    }
    .kicker {
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, monospace;
      font-size: 11px;
      letter-spacing: 0.16em;
      color: var(--accent-moss);
      text-transform: uppercase;
      margin-bottom: 12px;
    }
    h1 {
      font-size: 24px;
      font-weight: 500;
      line-height: 1.35;
      margin-bottom: 12px;
      text-wrap: balance;
    }
    .desc {
      font-size: 14px;
      line-height: 1.7;
      color: var(--ink-secondary);
      margin-bottom: 24px;
    }
    .actions {
      display: flex;
      flex-direction: column;
      gap: 12px;
    }
    .btn-primary, .btn-secondary {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 100%;
      min-block-size: 44px;
      padding: 12px 16px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, monospace;
      font-size: 13px;
      letter-spacing: 0.06em;
      text-decoration: none;
      cursor: pointer;
      transition: opacity 0.15s ease, border-color 0.15s ease;
    }
    .btn-primary {
      background-color: var(--accent-warm);
      color: #ffffff;
      border: 1px solid var(--accent-warm);
    }
    .btn-primary:hover { opacity: 0.92; }
    .btn-secondary {
      background-color: var(--bg-archive);
      color: var(--ink-primary);
      border: 1px solid var(--border-divider);
    }
    .btn-secondary:hover { border-color: var(--ink-primary); }
    .btn-primary:focus-visible, .btn-secondary:focus-visible {
      outline: 2px solid var(--accent-warm);
      outline-offset: 2px;
    }
    .copy-tip {
      min-height: 20px;
      margin-top: 8px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, monospace;
      font-size: 12px;
      color: var(--accent-moss);
      opacity: 0;
      transition: opacity 0.15s ease;
    }
    .copy-tip[data-visible] { opacity: 1; }
    .copy-tip[data-error] { color: var(--accent-warm); }
    @media (prefers-reduced-motion: reduce) {
      .btn-primary, .btn-secondary, .copy-tip { transition: none; }
    }
    .code-box {
      margin-top: 16px;
      padding: 10px 12px;
      background-color: var(--bg-archive);
      border: 1px solid var(--border-subtle);
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, monospace;
      font-size: 11px;
      line-height: 1.5;
      color: var(--ink-muted);
      word-break: break-all;
      user-select: all;
    }
    .colophon {
      margin-top: 20px;
      padding-top: 14px;
      border-top: 1px solid var(--border-subtle);
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, monospace;
      font-size: 11px;
      line-height: 1.6;
      color: var(--ink-muted);
    }
  </style>
</head>
<body>
  <main class="sheet">
    <div class="kicker">Vesper Auth · 通行证核验通过</div>
    <h1>正在唤起《散页》{{.PlatformLabel}}</h1>
    <p class="desc">
      主理人身份已核验完毕。系统已签发一枚 {{.ExpiresSeconds}} 秒内单次有效的配对工单，正在自动唤起本地客户端完成设备钥匙落盘。
    </p>

    <div class="actions">
      <a id="open-app-link" class="btn-primary" href="{{.DeepLinkURL}}">打开《散页》完成授权</a>
      <button id="copy-deeplink-btn" type="button" class="btn-secondary" command="--copy" commandfor="deeplink-code">复制一次性授权深链</button>
    </div>
    <p id="copy-tip" class="copy-tip" role="status" aria-live="polite"></p>

    <div class="code-box"><code id="deeplink-code">{{.DeepLinkText}}</code></div>

    <div class="colophon">
      <div>中枢地址：{{.HubURL}}</div>
      <div>若浏览器未自动弹出应用打开提示，请点击上方按钮，或复制深链回到《散页》客户端粘贴接入。</div>
    </div>
  </main>

  <script nonce="{{.CSPNonce}}">
    (function () {
      var openLink = document.getElementById('open-app-link');
      var tip = document.getElementById('copy-tip');
      var lastMessage = '';
      var tipTimer = null;

      function legacyCopy(text) {
        var field = document.createElement('textarea');
        field.value = text;
        field.setAttribute('readonly', '');
        field.style.position = 'fixed';
        field.style.insetBlockStart = '0';
        field.style.opacity = '0';
        document.body.appendChild(field);
        field.select();
        try {
          return document.execCommand('copy');
        } finally {
          field.remove();
        }
      }

      function report(button, ok) {
        if (tipTimer) clearTimeout(tipTimer);
        button.removeAttribute('data-copied');
        button.removeAttribute('data-failed');
        tip.removeAttribute('data-visible');
        tip.removeAttribute('data-error');

        var msg = ok ? '已复制一次性授权深链，请回到《散页》粘贴完成绑定' : '浏览器拒绝剪贴板写入，请长按或全选下方深链手动复制';
        if (msg === lastMessage) msg += '\u00A0';
        lastMessage = msg;

        button.setAttribute(ok ? 'data-copied' : 'data-failed', '');
        if (!ok) tip.setAttribute('data-error', '');
        tip.textContent = msg;
        tip.setAttribute('data-visible', '');

        tipTimer = setTimeout(function () {
          button.removeAttribute('data-copied');
          button.removeAttribute('data-failed');
          tip.removeAttribute('data-visible');
          tip.removeAttribute('data-error');
          tip.textContent = '';
        }, 3200);
      }

      async function performCopy(button, target) {
        if (!target) return;
        var text = (target.innerText || target.textContent || '').trim();
        if (!text) return;
        try {
          await navigator.clipboard.writeText(text);
          report(button, true);
        } catch (_err) {
          if (legacyCopy(text)) {
            report(button, true);
          } else {
            report(button, false);
          }
        }
      }

      if ('CommandEvent' in window) {
        document.addEventListener('command', function (event) {
          if (event.command && event.command.indexOf('--copy') === 0) {
            performCopy(event.source, event.target);
          }
        }, true);
      } else {
        document.addEventListener('click', function (event) {
          var btn = event.target && event.target.closest ? event.target.closest('button[command^="--copy"]') : null;
          if (!btn) return;
          var target = document.getElementById(btn.getAttribute('commandfor'));
          performCopy(btn, target);
        });
      }

      if (openLink) {
        var href = openLink.getAttribute('href');
        if (href && href.indexOf('looseleaf://pair?') === 0) {
          window.requestAnimationFrame(function () {
            window.location.assign(href);
          });
        }
      }
    })();
  </script>
</body>
</html>
`))

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
