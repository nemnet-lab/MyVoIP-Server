package config

import "testing"

func base(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MYVOIP_SECRET_KEY", "k")
	t.Setenv("MYVOIP_PUBLIC_URL", "http://192.168.1.20")
}

func TestDefaultsAllowPlainLANSetup(t *testing.T) {
	base(t)
	c, err := Load(false)
	if err != nil {
		t.Fatal(err)
	}
	if c.SIPTransport != "udp" || c.SIPProxyPort != 5060 || c.SIPSRTP != "optional" {
		t.Fatalf("%+v", c)
	}
}

func TestTLSDefaultPort(t *testing.T) {
	base(t)
	t.Setenv("SIP_TRANSPORT", "TLS")
	c, err := Load(false)
	if err != nil || c.SIPProxyPort != 5061 {
		t.Fatalf("%v %d", err, c.SIPProxyPort)
	}
}

func TestAllowedClientCIDRs(t *testing.T) {
	base(t)
	t.Setenv("MYVOIP_ALLOWED_CLIENT_CIDRS", "100.64.0.0/10, fd7a:115c:a1e0::/48")
	c, err := Load(false)
	if err != nil || len(c.AllowedClients) != 2 {
		t.Fatalf("%v %v", err, c.AllowedClients)
	}
	t.Setenv("MYVOIP_ALLOWED_CLIENT_CIDRS", "100.64.0.0")
	if _, err := Load(false); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
}

func TestRejectsUnknownValues(t *testing.T) {
	base(t)
	t.Setenv("SIP_TRANSPORT", "sctp")
	t.Setenv("SIP_SRTP", "always")
	t.Setenv("MYVOIP_PUBLIC_URL", "ftp://x")
	if _, err := Load(false); err == nil {
		t.Fatal("expected error")
	}
}

func TestWebConsoleSettings(t *testing.T) {
	base(t)
	c, err := Load(false)
	if err != nil || c.Web.Enabled {
		t.Fatalf("web console should be off by default: %v %+v", err, c.Web)
	}
	t.Setenv("DISCORD_CLIENT_ID", "id")
	t.Setenv("DISCORD_CLIENT_SECRET", "secret")
	if _, err := Load(false); err == nil {
		t.Fatal("web console without admins accepted")
	}
	t.Setenv("MYVOIP_ADMIN_DISCORD_IDS", "111, 222")
	t.Setenv("DISCORD_ROLE_IDS", "r1")
	if _, err := Load(false); err == nil {
		t.Fatal("roles without guild accepted")
	}
	t.Setenv("DISCORD_GUILD_ID", "g1")
	c, err = Load(false)
	if err != nil || !c.Web.Enabled || len(c.Web.AdminIDs) != 2 || c.Web.AdminIDs[1] != "222" || c.Web.MaxPhonesPerUser != 3 {
		t.Fatalf("%v %+v", err, c.Web)
	}
	// 利用者が設定できる PBX は、既定で Tailnet のアドレス帯だけ
	if len(c.Web.AllowedPBX) != 2 || c.Web.AllowedPBX[0].String() != "100.64.0.0/10" {
		t.Fatalf("AllowedPBX = %v", c.Web.AllowedPBX)
	}
	t.Setenv("MYVOIP_ALLOWED_PBX_CIDRS", "100.64.0.0")
	if _, err := Load(false); err == nil {
		t.Fatal("invalid PBX CIDR accepted")
	}
}

func TestRelayWithoutDefaultPBX(t *testing.T) {
	base(t)
	t.Setenv("RELAY_ENABLED", "true")
	// 利用者がそれぞれ自分の PBX を設定する構成では、既定の PBX はなくてもよい
	if _, err := Load(false); err != nil {
		t.Fatal(err)
	}
}
