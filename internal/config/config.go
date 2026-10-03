// Package config は環境変数から設定を読む。
package config

import (
	"errors"
	"fmt"
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

		TestCallSound: env("TEST_CALL_SOUND", "sound:hello-world"),
	}

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
	switch c.SIPSRTP {
	case "optional", "mandatory", "disabled":
	default:
		errs = append(errs, "SIP_SRTP must be optional, mandatory or disabled")
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
	// 呼出期限は 15〜60 秒（F-15）
	if c.RingTimeout < 15*time.Second || c.RingTimeout > 60*time.Second {
		errs = append(errs, "RING_TIMEOUT_SECONDS must be between 15 and 60")
	}
	if len(errs) > 0 {
		return c, errors.New(strings.Join(errs, "; "))
	}
	return c, nil
}
