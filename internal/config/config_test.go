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

func TestRejectsUnknownValues(t *testing.T) {
	base(t)
	t.Setenv("SIP_TRANSPORT", "sctp")
	t.Setenv("SIP_SRTP", "always")
	t.Setenv("MYVOIP_PUBLIC_URL", "ftp://x")
	if _, err := Load(false); err == nil {
		t.Fatal("expected error")
	}
}
