// Package config は環境変数から設定を読む。
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// PublicURL は iPhone から見た設定 URL（例 http://192.168.1.20 / https://voip.example.jp）
	PublicURL   string
	Listen      string
	DatabaseURL string
	SecretKey   string

	ARIURL      string
	ARIUser     string
	ARIPassword string
	ARIApp      string

	SIPDomain    string
	SIPProxyHost string
	SIPProxyPort int
	// SIPTransport は udp / tcp / tls。外部に公開しない LAN・VPN 内での利用を前提に非暗号化も許可する
	SIPTransport string
	// SIPSRTP は optional / mandatory / disabled
	SIPSRTP string

	APNSKeyFile  string
	APNSKeyID    string
	APNSTeamID   string
	APNSBundleID string

	RingTimeout       time.Duration
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	EnrollmentCodeTTL time.Duration
	HistoryRetention  time.Duration

	TestCallSound string

	// 中継モード：VoIP Push サーバー内の Asterisk が MikoPBX に内線として登録し、iPhone はその Asterisk に登録する
	RelayEnabled bool
	// RelayConfigDir は中継用 Asterisk の設定（pjsip_myvoip.conf）を書き出すディレクトリ
	RelayConfigDir string
	// UpstreamSIPHost / Port は既定の PBX の SIP の接続先（中継用 Asterisk から見たアドレス）。
	// 管理コマンドで登録した内線と、PBX を設定していない利用者の内線の登録先。利用者が自分の PBX を設定するなら空でもよい
	UpstreamSIPHost string
	UpstreamSIPPort int
	// APNSEndpoint は APNs の送信先の上書き（試験用。通常は空）
	APNSEndpoint string
	// AllowedClients は API に接続できるクライアントのアドレス帯（MYVOIP_ALLOWED_CLIENT_CIDRS）
	AllowedClients []netip.Prefix

	// Web 管理画面（DISCORD_CLIENT_ID を設定すると有効になる）
	Web WebConfig
}

// WebConfig は Discord でログインする Web 管理画面の設定。
type WebConfig struct {
	Enabled             bool
	DiscordClientID     string
	DiscordClientSecret string
	// DiscordGuildID を指定すると、そのサーバー（ギルド）のメンバーだけがログインできる
	DiscordGuildID string
	// DiscordRoleIDs を指定すると、そのロールのどれかを持つメンバーだけがログインできる
	DiscordRoleIDs []string
	// AllowedUserIDs はギルドに関係なくログインできる Discord ユーザー ID
	AllowedUserIDs []string
	// AdminIDs は管理者の Discord ユーザー ID（常にログインでき、管理画面を使える）
	AdminIDs []string
	// AllowedPBX は利用者が設定できる PBX のアドレス帯（MYVOIP_ALLOWED_PBX_CIDRS。既定は Tailnet）。
	// 利用者が入力したアドレスにこのサーバーから SIP を送るため、意図しない宛先（LAN 内の機器など）を防ぐ
	AllowedPBX []netip.Prefix
	// MOHMaxFiles は利用者1人が登録できる保留音の数、MOHMaxDuration は1つの保留音の長さの上限
	MOHMaxFiles    int
	MOHMaxDuration time.Duration
	// MaxPhonesPerUser は利用者1人が作成できる iPhone（内線）の上限
	MaxPhonesPerUser int
	SessionTTL       time.Duration
	SupportCodeTTL   time.Duration
	// SupportSessionTTL は管理者がパスコードで利用者の画面を開ける時間
	SupportSessionTTL time.Duration
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// list はカンマ区切りの値を分ける。
func list(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Load は環境変数を読む。`requireRuntime` が false なら管理コマンド用に最小限だけ検証する。
func Load(requireRuntime bool) (Config, error) {
	c := Config{
		PublicURL:   strings.TrimRight(env("MYVOIP_PUBLIC_URL", ""), "/"),
		Listen:      env("MYVOIP_LISTEN", ":8080"),
		DatabaseURL: env("DATABASE_URL", ""),
		SecretKey:   env("MYVOIP_SECRET_KEY", ""),

		ARIURL:      strings.TrimRight(env("ARI_URL", ""), "/"),
		ARIUser:     env("ARI_USER", ""),
		ARIPassword: env("ARI_PASSWORD", ""),
		ARIApp:      env("ARI_APP", "myvoip"),

		SIPDomain:    env("SIP_DOMAIN", ""),
		SIPProxyHost: env("SIP_PROXY_HOST", ""),
		SIPTransport: strings.ToLower(env("SIP_TRANSPORT", "udp")),
		SIPSRTP:      strings.ToLower(env("SIP_SRTP", "optional")),

		APNSKeyFile:  env("APNS_KEY_FILE", "/run/secrets/apns.p8"),
		APNSKeyID:    env("APNS_KEY_ID", ""),
		APNSTeamID:   env("APNS_TEAM_ID", ""),
		APNSBundleID: env("APNS_BUNDLE_ID", "net.nemnet-lab.myvoip"),

		RingTimeout:       time.Duration(envInt("RING_TIMEOUT_SECONDS", 30)) * time.Second,
		AccessTokenTTL:    time.Hour,
		RefreshTokenTTL:   time.Duration(envInt("REFRESH_TOKEN_DAYS", 180)) * 24 * time.Hour,
		EnrollmentCodeTTL: 10 * time.Minute,
		HistoryRetention:  180 * 24 * time.Hour,

		TestCallSound: env("TEST_CALL_SOUND", "sound:vm-intro"),

		RelayEnabled:    strings.EqualFold(env("RELAY_ENABLED", "false"), "true"),
		RelayConfigDir:  env("RELAY_CONFIG_DIR", "/srv/asterisk-myvoip"),
		UpstreamSIPHost: env("UPSTREAM_SIP_HOST", ""),
		UpstreamSIPPort: envInt("UPSTREAM_SIP_PORT", 5060),
		APNSEndpoint:    strings.TrimRight(env("APNS_ENDPOINT", ""), "/"),
	}

	c.Web = WebConfig{
		DiscordClientID:     env("DISCORD_CLIENT_ID", ""),
		DiscordClientSecret: env("DISCORD_CLIENT_SECRET", ""),
		DiscordGuildID:      env("DISCORD_GUILD_ID", ""),
		DiscordRoleIDs:      list(env("DISCORD_ROLE_IDS", "")),
		AllowedUserIDs:      list(env("DISCORD_ALLOWED_USER_IDS", "")),
		AdminIDs:            list(env("MYVOIP_ADMIN_DISCORD_IDS", "")),
		MaxPhonesPerUser:    envInt("MYVOIP_MAX_PHONES_PER_USER", 3),
		MOHMaxFiles:         envInt("MYVOIP_MOH_MAX_FILES_PER_USER", 5),
		MOHMaxDuration:      time.Duration(envInt("MYVOIP_MOH_MAX_SECONDS", 300)) * time.Second,
		SessionTTL:          time.Duration(envInt("WEB_SESSION_HOURS", 168)) * time.Hour,
		SupportCodeTTL:      time.Duration(envInt("SUPPORT_PASSCODE_MINUTES", 30)) * time.Minute,
		SupportSessionTTL:   time.Duration(envInt("SUPPORT_SESSION_MINUTES", 60)) * time.Minute,
	}
	c.Web.Enabled = c.Web.DiscordClientID != ""

	var errs []string
	need := func(name, v string) {
		if v == "" {
			errs = append(errs, name+" is required")
		}
	}
	need("DATABASE_URL", c.DatabaseURL)
	need("MYVOIP_SECRET_KEY", c.SecretKey)
	need("MYVOIP_PUBLIC_URL", c.PublicURL)
	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, "MYVOIP_PUBLIC_URL must be an http:// or https:// URL")
		}
	}
	switch c.SIPTransport {
	case "udp", "tcp":
		c.SIPProxyPort = envInt("SIP_PROXY_PORT", 5060)
	case "tls":
		c.SIPProxyPort = envInt("SIP_PROXY_PORT", 5061)
	default:
		errs = append(errs, "SIP_TRANSPORT must be udp, tcp or tls")
	}
	for _, raw := range strings.Split(env("MYVOIP_ALLOWED_CLIENT_CIDRS", ""), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			errs = append(errs, "MYVOIP_ALLOWED_CLIENT_CIDRS has an invalid CIDR: "+raw)
			continue
		}
		c.AllowedClients = append(c.AllowedClients, prefix.Masked())
	}
	switch c.SIPSRTP {
	case "optional", "mandatory", "disabled":
	default:
		errs = append(errs, "SIP_SRTP must be optional, mandatory or disabled")
	}
	if c.RelayEnabled {
		if c.SIPTransport != "udp" {
			errs = append(errs, "relay mode supports SIP_TRANSPORT=udp only")
		}
	}
	if requireRuntime {
		need("ARI_URL", c.ARIURL)
		need("ARI_USER", c.ARIUser)
		need("ARI_PASSWORD", c.ARIPassword)
		need("SIP_DOMAIN", c.SIPDomain)
		need("APNS_KEY_ID", c.APNSKeyID)
		need("APNS_TEAM_ID", c.APNSTeamID)
		if _, err := os.Stat(c.APNSKeyFile); err != nil {
			errs = append(errs, fmt.Sprintf("APNs key file not readable: %s", c.APNSKeyFile))
		}
	}
	for _, raw := range list(env("MYVOIP_ALLOWED_PBX_CIDRS", "100.64.0.0/10,fd7a:115c:a1e0::/48")) {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			errs = append(errs, "MYVOIP_ALLOWED_PBX_CIDRS has an invalid CIDR: "+raw)
			continue
		}
		c.Web.AllowedPBX = append(c.Web.AllowedPBX, prefix.Masked())
	}
	if c.Web.Enabled {
		need("DISCORD_CLIENT_SECRET", c.Web.DiscordClientSecret)
		if len(c.Web.AdminIDs) == 0 {
			errs = append(errs, "MYVOIP_ADMIN_DISCORD_IDS is required when the web console is enabled")
		}
		if c.Web.MaxPhonesPerUser < 1 || c.Web.MaxPhonesPerUser > 20 {
			errs = append(errs, "MYVOIP_MAX_PHONES_PER_USER must be between 1 and 20")
		}
		if c.Web.MOHMaxFiles < 0 || c.Web.MOHMaxFiles > 50 {
			errs = append(errs, "MYVOIP_MOH_MAX_FILES_PER_USER must be between 0 and 50")
		}
		if c.Web.MOHMaxDuration < 10*time.Second || c.Web.MOHMaxDuration > 20*time.Minute {
			errs = append(errs, "MYVOIP_MOH_MAX_SECONDS must be between 10 and 1200")
		}
		if c.Web.DiscordGuildID == "" && len(c.Web.DiscordRoleIDs) > 0 {
			errs = append(errs, "DISCORD_ROLE_IDS requires DISCORD_GUILD_ID")
		}
	}
	// 呼出期限は 15〜60 秒（F-15）
	if c.RingTimeout < 15*time.Second || c.RingTimeout > 60*time.Second {
		errs = append(errs, "RING_TIMEOUT_SECONDS must be between 15 and 60")
	}
	if len(errs) > 0 {
		return c, errors.New(strings.Join(errs, "; "))
	}
	return c, nil
}
