package relay

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

func TestRenderRelayConfig(t *testing.T) {
	box, _ := secret.NewBox(secret.NewKey())
	exts := []store.Extension{{
		Number: "9002", SIPUsername: "9002", SIPAuthUsername: "9002",
		SIPPasswordEnc: box.Seal("up;secret"), MobilePasswordEnc: box.Seal("mobile-pw"),
	}}
	out, err := Render(exts, box, Options{UpstreamHost: "100.64.0.10", UpstreamPort: 5060,
		AllowedClients: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[myvoip-up-9002]\ntype=registration",
		"server_uri=sip:100.64.0.10:5060",
		"client_uri=sip:9002@100.64.0.10",
		"line=yes",
		`password=up\;secret`, // ; はエスケープする
		"[9002]\ntype=endpoint",
		"context=from-mobile",
		"password=mobile-pw",
		"permit=100.64.0.0/10",
		"allow=alaw,ulaw",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "allow=ulaw,alaw") {
		t.Error("codec order must prefer alaw")
	}
}

func TestRenderRejectsUnsafeNames(t *testing.T) {
	box, _ := secret.NewBox(secret.NewKey())
	exts := []store.Extension{{Number: "90 02", SIPUsername: "9002", SIPPasswordEnc: box.Seal("x"), MobilePasswordEnc: box.Seal("y")}}
	if _, err := Render(exts, box, Options{UpstreamHost: "h", UpstreamPort: 5060}); err == nil {
		t.Fatal("unsafe extension number must be rejected")
	}
}
