package relay

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		"rtp_timeout=60", // 強制終了などで BYE が届かなかった通話を残さない
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

type recordingReloader struct{ modules []string }

func (r *recordingReloader) ReloadModule(_ context.Context, name string) error {
	r.modules = append(r.modules, name)
	return nil
}

func TestSyncWritesUsersHoldMusic(t *testing.T) {
	ctx := context.Background()
	box, _ := secret.NewBox(secret.NewKey())
	st := store.NewMemory()
	dir := t.TempDir()
	now := time.Now()
	for _, id := range []string{"u-1", "u-2"} {
		if _, err := st.UpsertDiscordUser(ctx, store.User{ID: id, DiscordID: "d" + id}, now); err != nil {
			t.Fatal(err)
		}
		_ = st.SetUserPBX(ctx, id, "100.64.0.21", 5060)
	}
	for _, e := range []store.Extension{
		{Number: "mv1", SIPUsername: "201", SIPPasswordEnc: box.Seal("a"), MobilePasswordEnc: box.Seal("m"), OwnerUserID: "u-1", PBXExtension: "201"},
		{Number: "mv2", SIPUsername: "202", SIPPasswordEnc: box.Seal("b"), MobilePasswordEnc: box.Seal("m"), OwnerUserID: "u-2", PBXExtension: "202"},
	} {
		if err := st.CreateOwnedExtension(ctx, e, 5, now); err != nil {
			t.Fatal(err)
		}
	}
	const mohID = "0f8fad5b-d9cb-469f-a165-70867728950e"
	if err := st.CreateMOH(ctx, store.MOHFile{ID: mohID, OwnerUserID: "u-1", Name: "曲", DurationMS: 1000, Data: []byte("RIFF-wav-data")}, 5); err != nil {
		t.Fatal(err)
	}
	// 他人の保留音は割り当てられない
	if err := st.SetExtensionMOH(ctx, "mv2", mohID); err == nil {
		t.Fatal("assigned another user's hold music")
	}
	if err := st.SetExtensionMOH(ctx, "mv1", mohID); err != nil {
		t.Fatal(err)
	}
	reloader := &recordingReloader{}
	result, err := Sync(ctx, st, box, Options{}, dir, reloader)
	if err != nil || len(result.Skipped) > 0 {
		t.Fatal(err, result.Skipped)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "moh", mohID, "hold.wav")); err != nil || string(b) != "RIFF-wav-data" {
		t.Fatalf("hold music file: %q %v", b, err)
	}
	mohConf, _ := os.ReadFile(filepath.Join(dir, MOHFileName))
	if !strings.Contains(string(mohConf), "[myvoip-moh-"+mohID+"]\nmode=files\ndirectory=/etc/asterisk/myvoip/moh/"+mohID+"\n") {
		t.Fatalf("musiconhold conf:\n%s", mohConf)
	}
	if strings.Contains(string(mohConf), "曲") {
		t.Fatal("user-supplied name must not appear in the Asterisk configuration")
	}
	pjsip, _ := os.ReadFile(filepath.Join(dir, FileName))
	cfg := string(pjsip)
	// iPhone 用エンドポイント（mv1）にだけ付く
	mobile := cfg[strings.Index(cfg, "[mv1]\ntype=endpoint"):]
	mobile = mobile[:strings.Index(mobile, "\n\n; =====")+1]
	if !strings.Contains(mobile, "moh_suggest=myvoip-moh-"+mohID) || strings.Count(cfg, "moh_suggest=") != 1 {
		t.Fatalf("moh_suggest not only on mv1's mobile endpoint:\n%s", cfg)
	}
	if strings.Join(reloader.modules, ",") != "res_musiconhold.so,res_pjsip.so,res_pjsip_outbound_registration.so" {
		t.Fatalf("reloaded %v", reloader.modules)
	}
	// 消すと割り当てが外れ、音声とクラスも消える
	if err := st.DeleteMOH(ctx, mohID); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(ctx, st, box, Options{}, dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "moh", mohID)); !os.IsNotExist(err) {
		t.Fatalf("hold music directory remains: %v", err)
	}
	pjsip, _ = os.ReadFile(filepath.Join(dir, FileName))
	mohConf, _ = os.ReadFile(filepath.Join(dir, MOHFileName))
	if strings.Contains(string(pjsip), "moh_suggest") || strings.Contains(string(mohConf), mohID) {
		t.Fatal("deleted hold music is still configured")
	}
}
