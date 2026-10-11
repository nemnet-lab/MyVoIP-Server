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
	out, skipped := Render(exts, box, Options{UpstreamHost: "100.64.0.10", UpstreamPort: 5060,
		AllowedClients: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}})
	if len(skipped) > 0 {
		t.Fatal(skipped)
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
		"identify_by=ip", // PBX 側のエンドポイントはユーザー名では一致させない
		"qualify_frequency=30",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "allow=ulaw,alaw") {
		t.Error("codec order must prefer alaw")
	}
}

func TestRenderSkipsOnlyBrokenExtensions(t *testing.T) {
	box, _ := secret.NewBox(secret.NewKey())
	good := store.Extension{Number: "9002", SIPUsername: "9002", SIPPasswordEnc: box.Seal("x"), MobilePasswordEnc: box.Seal("y")}
	exts := []store.Extension{
		{Number: "90 02", SIPUsername: "9002", SIPPasswordEnc: box.Seal("x"), MobilePasswordEnc: box.Seal("y")},
		{Number: "mv1", SIPUsername: "201", SIPPasswordEnc: box.Seal("bad\npassword"), MobilePasswordEnc: box.Seal("y")},
		{Number: "mv2", SIPUsername: "202", SIPPasswordEnc: box.Seal("x"), MobilePasswordEnc: box.Seal("y"), OwnerUserID: "u-bad"},
		good,
	}
	out, skipped := Render(exts, box, Options{UpstreamHost: "h", UpstreamPort: 5060,
		UserPBX: map[string]Upstream{"u-bad": {Host: "1.2.3.4\n[x]", Port: 5060}}})
	if len(skipped) != 3 {
		t.Fatalf("skipped = %v", skipped)
	}
	if !strings.Contains(out, "[myvoip-up-9002]") || strings.Contains(out, "[myvoip-up-mv1]") || strings.Contains(out, "[x]") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestRenderRegistersEachUsersPBX(t *testing.T) {
	box, _ := secret.NewBox(secret.NewKey())
	exts := []store.Extension{
		// 別々の PBX に同じ内線番号 201 がある
		{Number: "mv1", SIPUsername: "201", SIPPasswordEnc: box.Seal("a"), MobilePasswordEnc: box.Seal("m"), OwnerUserID: "u-1"},
		{Number: "mv2", SIPUsername: "201", SIPPasswordEnc: box.Seal("b"), MobilePasswordEnc: box.Seal("m"), OwnerUserID: "u-2"},
		// PBX を設定していない利用者と持ち主のいない内線は既定の PBX
		{Number: "9002", SIPUsername: "9002", SIPPasswordEnc: box.Seal("c"), MobilePasswordEnc: box.Seal("m"), OwnerUserID: "u-3"},
		{Number: "mv4", SIPUsername: "6001", SIPPasswordEnc: box.Seal("d"), MobilePasswordEnc: box.Seal("m"), OwnerUserID: "u-4"},
	}
	out, skipped := Render(exts, box, Options{UpstreamHost: "100.64.0.10", UpstreamPort: 5060, UserPBX: map[string]Upstream{
		"u-1": {Host: "100.64.0.21", Port: 5060},
		"u-2": {Host: "100.64.0.22", Port: 5080},
		"u-4": {Host: "fd7a:115c:a1e0::4", Port: 5060},
	}})
	if len(skipped) > 0 {
		t.Fatal(skipped)
	}
	for _, want := range []string{
		"[myvoip-up-mv1]\ntype=registration\ntransport=transport-udp\noutbound_auth=myvoip-up-mv1-auth\nserver_uri=sip:100.64.0.21:5060\nclient_uri=sip:201@100.64.0.21",
		"[myvoip-up-mv2]\ntype=registration\ntransport=transport-udp\noutbound_auth=myvoip-up-mv2-auth\nserver_uri=sip:100.64.0.22:5080\nclient_uri=sip:201@100.64.0.22",
		"server_uri=sip:100.64.0.10:5060\nclient_uri=sip:9002@100.64.0.10",
		"server_uri=sip:[fd7a:115c:a1e0::4]:5060\nclient_uri=sip:6001@[fd7a:115c:a1e0::4]",
		"set_var=MYVOIP_EXT=mv2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// 既定の PBX もなければ、利用者の PBX がない内線は書かない
	if _, skipped := Render(exts[2:3], box, Options{}); len(skipped) != 1 {
		t.Fatalf("extension without PBX must be skipped: %v", skipped)
	}
}
