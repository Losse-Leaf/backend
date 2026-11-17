package config

import (
	"bufio"
	"crypto/subtle"
	"os"
	"strconv"
	"strings"
	"time"
)

type R2Config struct {
	AccountID            string
	AccessKeyID          string
	SecretAccessKey      string
	BucketName           string
	Endpoint             string
	PublicDomain         string
	PresignExpireSeconds int // 临时签名 URL 有效期 (秒)，默认 1800 秒 (30 分钟)
}

type OIDCConfig struct {
	Enabled      bool
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	AllowedSubs  []string
}

// IsSubAllowed 核验 OIDC sub 是否命中主理人白名单；未配置白名单时默认放行非空 sub。
func (o OIDCConfig) IsSubAllowed(sub string) bool {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return false
	}
	if len(o.AllowedSubs) == 0 {
		return true
	}
	matched := false
	for _, allowed := range o.AllowedSubs {
		if subtle.ConstantTimeCompare([]byte(sub), []byte(allowed)) == 1 {
			matched = true
		}
	}
	return matched
}

type Config struct {
	DBPath              string
	ListenHost          string
	Port                string
	SiteURL             string
	OIDC                OIDCConfig
	InternalBridgeToken string
	R2                  R2Config
}

var AppConfig Config

// Version 应用程序版本号，在构建时通过 -ldflags 注入 (RC-66)
var Version = "dev"

// BeijingLocation 全项目统一时区基准：中国北京时间 (UTC+8, 固定偏移，不依赖系统 tzdata) (RC-134)
// 部署目标为海外 VPS (系统时区通常为 UTC)，所有「无时区后缀」输入一律按北京时间解释，
// 所有年月日边界计算一律以北京时间切分，保证跨时区部署下时间显示零漂移。
var BeijingLocation = time.FixedZone("CST", 8*3600)

// NowInBeijing 返回当前时刻的北京时间表示
func NowInBeijing() time.Time {
	return time.Now().In(BeijingLocation)
}

// TodayInBeijing 返回北京时间当天日期 (YYYY-MM-DD)
func TodayInBeijing() string {
	return NowInBeijing().Format("2006-01-02")
}

// loadDotEnv 轻量读取当前工作目录或 backend/ 下的 .env 文件
func loadDotEnv() {
	file, err := os.Open(".env")
	if err != nil {
		file, err = os.Open("backend/.env")
		if err != nil {
			return
		}
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)
		// 仅在环境变量未显式设置时填充
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

const DefaultDBPath = "data/looseleaf.db"

func LoadConfig() Config {
	loadDotEnv()

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = DefaultDBPath
	}

	port := os.Getenv("PORT")

	// 命令行参数最高优先级覆盖 (支持 ./looseleaf -port 8080 或 ./looseleaf -p 8080 或 ./looseleaf 8080)
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if (arg == "-p" || arg == "-port" || arg == "--port") && i+1 < len(os.Args) {
			port = os.Args[i+1]
			i++
		} else if strings.HasPrefix(arg, "-port=") {
			port = strings.TrimPrefix(arg, "-port=")
		} else if strings.HasPrefix(arg, "--port=") {
			port = strings.TrimPrefix(arg, "--port=")
		} else if strings.HasPrefix(arg, "-p=") {
			port = strings.TrimPrefix(arg, "-p=")
		} else if val, err := strconv.Atoi(arg); err == nil && val > 0 && val <= 65535 {
			port = arg
		}
	}

	if port == "" {
		port = "8088"
	}

	listenHost := strings.TrimSpace(os.Getenv("LISTEN_HOST"))
	if listenHost == "" {
		listenHost = "0.0.0.0"
	}

	siteURL := strings.TrimRight(strings.TrimSpace(os.Getenv("SITE_URL")), "/")

	oidcIssuer := strings.TrimRight(strings.TrimSpace(os.Getenv("OIDC_ISSUER")), "/")
	oidcClientID := strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID"))
	oidcClientSecret := strings.TrimSpace(os.Getenv("OIDC_CLIENT_SECRET"))
	oidcRedirectURI := strings.TrimSpace(os.Getenv("OIDC_REDIRECT_URI"))

	var allowedSubs []string
	if rawAllowed := strings.TrimSpace(os.Getenv("OIDC_ALLOWED_SUB")); rawAllowed != "" {
		seen := make(map[string]bool)
		for _, part := range strings.Split(rawAllowed, ",") {
			sub := strings.TrimSpace(part)
			if sub != "" && !seen[sub] {
				seen[sub] = true
				allowedSubs = append(allowedSubs, sub)
			}
		}
	}

	oidcEnabled := oidcIssuer != "" && oidcClientID != ""
	if rawEnabled := strings.TrimSpace(os.Getenv("OIDC_ENABLED")); rawEnabled != "" {
		if parsed, err := strconv.ParseBool(rawEnabled); err == nil {
			oidcEnabled = parsed
		}
	}

	oidcCfg := OIDCConfig{
		Enabled:      oidcEnabled,
		Issuer:       oidcIssuer,
		ClientID:     oidcClientID,
		ClientSecret: oidcClientSecret,
		RedirectURI:  oidcRedirectURI,
		AllowedSubs:  allowedSubs,
	}

	internalBridgeToken := strings.TrimSpace(os.Getenv("INTERNAL_BRIDGE_TOKEN"))

	accountID := strings.TrimSpace(os.Getenv("R2_ACCOUNT_ID"))
	endpoint := strings.TrimSpace(os.Getenv("R2_ENDPOINT"))
	if endpoint == "" && accountID != "" {
		endpoint = "https://" + accountID + ".r2.cloudflarestorage.com"
	}

	expireSec := 1800 // 默认 30 分钟 (1800 秒)
	if s := os.Getenv("R2_PRESIGN_EXPIRES"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			expireSec = v
		}
	}

	r2Cfg := R2Config{
		AccountID:            accountID,
		AccessKeyID:          strings.TrimSpace(os.Getenv("R2_ACCESS_KEY_ID")),
		SecretAccessKey:      strings.TrimSpace(os.Getenv("R2_SECRET_ACCESS_KEY")),
		BucketName:           strings.TrimSpace(os.Getenv("R2_BUCKET_NAME")),
		Endpoint:             endpoint,
		PublicDomain:         strings.TrimRight(strings.TrimSpace(os.Getenv("R2_PUBLIC_DOMAIN")), "/"),
		PresignExpireSeconds: expireSec,
	}

	AppConfig = Config{
		DBPath:              dbPath,
		ListenHost:          listenHost,
		Port:                port,
		SiteURL:             siteURL,
		OIDC:                oidcCfg,
		InternalBridgeToken: internalBridgeToken,
		R2:                  r2Cfg,
	}

	return AppConfig
}

func (r *R2Config) IsConfigured() bool {
	return r.AccessKeyID != "" && r.SecretAccessKey != "" && r.BucketName != "" && r.Endpoint != ""
}
