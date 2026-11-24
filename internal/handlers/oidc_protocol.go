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
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"loose-leaf/internal/config"
	"loose-leaf/internal/middleware"

	"github.com/gin-gonic/gin"
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
	oidcStates     = newOIDCStateManager()
	oidcHTTPClient = &http.Client{Timeout: 10 * time.Second}
	oidcCacheMu    sync.RWMutex
	discoveryCache = make(map[string]cachedDiscovery)
	jwksCacheByURI = make(map[string]cachedJWKS)
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
	Iss               string       `json:"iss"`
	Sub               string       `json:"sub"`
	Aud               audienceList `json:"aud"`
	Azp               string       `json:"azp"`
	Exp               int64        `json:"exp"`
	Iat               int64        `json:"iat"`
	Nbf               int64        `json:"nbf"`
	Nonce             string       `json:"nonce"`
	Email             string       `json:"email"`
	Name              string       `json:"name"`
	PreferredUsername string       `json:"preferred_username"`
	Nickname          string       `json:"nickname"`
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
