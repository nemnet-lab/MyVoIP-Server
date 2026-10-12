package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nemnet-lab/MyVoIP-Server/internal/api"
	"github.com/nemnet-lab/MyVoIP-Server/internal/config"
	"github.com/nemnet-lab/MyVoIP-Server/internal/relay"
	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

// fakeDiscord は Discord の OAuth2 と API の代わり。認可コードを Discord のユーザー ID として扱う。
type fakeDiscord struct {
	// members はギルドのメンバーとロール
	members map[string][]string
}

func (f *fakeDiscord) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		id, secretValue, ok := r.BasicAuth()
		if !ok || id != "client-id" || secretValue != "client-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.PostFormValue("redirect_uri") != "http://myvoip.test/auth/discord/callback" {
			t.Errorf("redirect_uri = %q", r.PostFormValue("redirect_uri"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + r.PostFormValue("code")})
	})
	user := func(r *http.Request) string {
		return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
	}
	mux.HandleFunc("GET /users/@me", func(w http.ResponseWriter, r *http.Request) {
		id := user(r)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "username": id + "_name", "global_name": "Name " + id})
	})
	mux.HandleFunc("GET /users/@me/guilds/{guild}/member", func(w http.ResponseWriter, r *http.Request) {
		roles, ok := f.members[user(r)]
		if !ok || r.PathValue("guild") != "guild-1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"roles": roles})
	})
	return mux
}

type harness struct {
	t     *testing.T
	st    *store.Memory
	box   *secret.Box
	web   http.Handler
	api   http.Handler
	syncs int
	// relayDir は中継用 Asterisk の設定の書き出し先（実際の relay.Sync で書く）
	relayDir string
}

func newHarness(t *testing.T, roles []string) *harness {
	t.Helper()
	discord := &fakeDiscord{members: map[string][]string{"alice": {"role-1"}, "bob": {"role-1"}, "carol": {"other"}}}
	ds := httptest.NewServer(discord.handler(t))
	t.Cleanup(ds.Close)

	box, err := secret.NewBox(secret.NewKey())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, st: store.NewMemory(), box: box, relayDir: t.TempDir()}
	ctx := context.Background()
	// 管理コマンドで登録した既存の内線（既定の PBX へ登録する）
	if err := h.st.UpsertExtension(ctx, store.Extension{
		Number: "9002", SIPUsername: "9002", SIPAuthUsername: "9002", SIPPasswordEnc: box.Seal("pbx-9002"), Endpoint: "9002", RingTimeoutSeconds: 30,
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	syncer := func(ctx context.Context) error {
		h.syncs++
		_, err := relay.Sync(ctx, h.st, box, relay.Options{UpstreamHost: "100.64.0.10", UpstreamPort: 5060}, h.relayDir, nil)
		return err
	}
	srv, err := New(h.st, box, Config{
		PublicURL: "http://myvoip.test", EnrollmentCodeTTL: 10 * time.Minute, Relay: true,
		Web: config.WebConfig{
			Enabled: true, DiscordClientID: "client-id", DiscordClientSecret: "client-secret",
			DiscordGuildID: "guild-1", DiscordRoleIDs: roles, AdminIDs: []string{"admin"},
			AllowedPBX:       []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")},
			MaxPhonesPerUser: 2, SessionTTL: time.Hour, SupportCodeTTL: 30 * time.Minute, SupportSessionTTL: time.Hour,
			MOHMaxFiles: 2, MOHMaxDuration: 30 * time.Second,
		},
		DiscordAPI: ds.URL, DiscordAuthorizeURL: "https://discord.example/oauth2/authorize",
	}, syncer, log)
	if err != nil {
		t.Fatal(err)
	}
	h.web = srv.Handler()
	h.api = api.New(h.st, nil, box, api.Config{
		SIPDomain: "relay.example", SIPTransport: "udp", BundleID: "net.example.app",
		AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RingTimeout: 30 * time.Second, Relay: true, Web: h.web,
	}, log).Handler()
	return h
}

func (h *harness) relayConfig() string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.relayDir, relay.FileName))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// client はブラウザの代わり（Cookie と CSRF トークンを持つ）。
type client struct {
	h       *harness
	cookies map[string]*http.Cookie
	csrf    string
}

func (h *harness) browser() *client { return &client{h: h, cookies: map[string]*http.Cookie{}} }

func (c *client) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.send(req)
}

func (c *client) send(req *http.Request) *httptest.ResponseRecorder {
	req.RemoteAddr = "127.0.0.1:50000"
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	c.h.api.ServeHTTP(rec, req)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge < 0 {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck
		}
	}
	return rec
}

// post は CSRF トークン付きで送る。
func (c *client) post(path string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", c.csrf)
	return c.do(http.MethodPost, path, form)
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// login は Discord でのログインをたどり、結果のリダイレクト先を返す。
func (c *client) login(discordID string) string {
	c.h.t.Helper()
	rec := c.do(http.MethodGet, "/auth/discord", nil)
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "discord.example" {
		c.h.t.Fatalf("authorize redirect = %q", rec.Header().Get("Location"))
	}
	state := loc.Query().Get("state")
	rec = c.do(http.MethodGet, "/auth/discord/callback?code="+discordID+"&state="+url.QueryEscape(state), nil)
	target := rec.Header().Get("Location")
	if target == "/" {
		page := c.do(http.MethodGet, "/", nil)
		m := csrfPattern.FindStringSubmatch(page.Body.String())
		if m == nil {
			c.h.t.Fatalf("no csrf token on dashboard: %d %s", page.Code, page.Body.String())
		}
		c.csrf = m[1]
	}
	return target
}

func (h *harness) userByDiscord(id string) store.User {
	h.t.Helper()
	users, _ := h.st.ListUsers(context.Background())
	for _, u := range users {
		if u.DiscordID == id {
			return u
		}
	}
	h.t.Fatalf("user %s not found", id)
	return store.User{}
}

func TestLoginChecksGuildAndState(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.browser().login("alice"); got != "/" {
		t.Fatalf("member login -> %q", got)
	}
	if got := h.browser().login("stranger"); got != "/login?e=denied" {
		t.Fatalf("non-member login -> %q", got)
	}
	// 管理者はギルドのメンバーでなくてもログインできる
	if got := h.browser().login("admin"); got != "/" {
		t.Fatalf("admin login -> %q", got)
	}
	// state が一致しなければ拒否する
	c := h.browser()
	c.do(http.MethodGet, "/auth/discord", nil)
	rec := c.do(http.MethodGet, "/auth/discord/callback?code=alice&state=forged", nil)
	if rec.Header().Get("Location") != "/login?e=failed" {
		t.Fatalf("forged state -> %q", rec.Header().Get("Location"))
	}
	if _, ok := c.cookies[sessionCookie]; ok {
		t.Fatal("session created with forged state")
	}
	// ログインしていなければログイン画面へ
	if rec := h.browser().do(http.MethodGet, "/", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous dashboard -> %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestLoginChecksRole(t *testing.T) {
	h := newHarness(t, []string{"role-1"})
	if got := h.browser().login("alice"); got != "/" {
		t.Fatalf("member with role -> %q", got)
	}
	if got := h.browser().login("carol"); got != "/login?e=denied" {
		t.Fatalf("member without role -> %q", got)
	}
}

func TestDisabledUserCannotLogIn(t *testing.T) {
	h := newHarness(t, nil)
	c := h.browser()
	c.login("alice")
	alice := h.userByDiscord("alice")
	admin := h.browser()
	admin.login("admin")
	if rec := admin.post("/admin/users/"+alice.ID+"/disabled", url.Values{"disabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("disable -> %d", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/", nil); rec.Header().Get("Location") != "/login" {
		t.Fatalf("disabled session still works: %d", rec.Code)
	}
	if got := h.browser().login("alice"); got != "/login?e=disabled" {
		t.Fatalf("disabled login -> %q", got)
	}
}

// setPBX は利用者の PBX を保存し、iPhone を追加できるようにする。
func (c *client) setPBX(host string) {
	c.h.t.Helper()
	if rec := c.post("/pbx", url.Values{"host": {host}, "port": {"5060"}}); rec.Code != http.StatusSeeOther {
		c.h.t.Fatalf("save pbx %s -> %d %s", host, rec.Code, rec.Body.String())
	}
}

func (c *client) addPhone(label, ext, password string) *httptest.ResponseRecorder {
	return c.post("/phones", url.Values{"label": {label}, "pbx_extension": {ext}, "password": {password}})
}

func (h *harness) phones(discordID string) []store.Extension {
	h.t.Helper()
	list, err := h.st.ListExtensionsByOwner(context.Background(), h.userByDiscord(discordID).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func TestPBXSettingIsValidated(t *testing.T) {
	h := newHarness(t, nil)
	c := h.browser()
	c.login("alice")
	if rec := c.addPhone("x", "201", "pw"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "PBX の設定") {
		t.Fatalf("add before PBX -> %d", rec.Code)
	}
	for _, host := range []string{"192.168.1.10", "127.0.0.1", "pbx.example.com", "100.64.0.1\n[x]", ""} {
		if rec := c.post("/pbx", url.Values{"host": {host}}); rec.Code != http.StatusBadRequest {
			t.Fatalf("host %q -> %d", host, rec.Code)
		}
	}
	if rec := c.post("/pbx", url.Values{"host": {"100.64.0.21"}, "port": {"70000"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad port -> %d", rec.Code)
	}
	c.setPBX("100.64.0.21")
	if u := h.userByDiscord("alice"); u.PBXHost != "100.64.0.21" || u.PBXPort != 5060 {
		t.Fatalf("pbx = %s:%d", u.PBXHost, u.PBXPort)
	}
	if h.syncs != 1 {
		t.Fatalf("relay syncs = %d", h.syncs)
	}
}

func TestEachUsersPhonesRegisterToTheirOwnPBX(t *testing.T) {
	h := newHarness(t, nil)
	alice, bob := h.browser(), h.browser()
	alice.login("alice")
	bob.login("bob")
	alice.setPBX("100.64.0.21")
	bob.setPBX("100.64.0.22")
	if rec := alice.addPhone("仕事用", "201", "alice-201"); rec.Code != http.StatusSeeOther {
		t.Fatalf("add -> %d %s", rec.Code, rec.Body.String())
	}
	// 同じ内線を2台目に使うことはできない
	if rec := alice.addPhone("重複", "201", "pw"); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate -> %d", rec.Code)
	}
	if rec := alice.addPhone("私用", "202", "alice-202"); rec.Code != http.StatusSeeOther {
		t.Fatalf("add 2 -> %d", rec.Code)
	}
	if rec := alice.addPhone("3台目", "203", "pw"); rec.Code != http.StatusConflict {
		t.Fatalf("over limit -> %d", rec.Code)
	}
	// 別の PBX に同じ内線番号 201 があってもよい
	if rec := bob.addPhone("", "201", "bob-201"); rec.Code != http.StatusSeeOther {
		t.Fatalf("bob add -> %d %s", rec.Code, rec.Body.String())
	}
	a, b := h.phones("alice"), h.phones("bob")
	if len(a) != 2 || len(b) != 1 || a[0].Number == b[0].Number || b[0].PBXExtension != "201" || b[0].Label != "iPhone" {
		t.Fatalf("alice=%+v bob=%+v", a, b)
	}
	if pw, _ := h.box.Open(b[0].SIPPasswordEnc); pw != "bob-201" {
		t.Fatal("password not stored")
	}
	cfg := h.relayConfig()
	for _, want := range []string{
		"server_uri=sip:100.64.0.21:5060\nclient_uri=sip:201@100.64.0.21",
		"server_uri=sip:100.64.0.21:5060\nclient_uri=sip:202@100.64.0.21",
		"server_uri=sip:100.64.0.22:5060\nclient_uri=sip:201@100.64.0.22",
		"server_uri=sip:100.64.0.10:5060\nclient_uri=sip:9002@100.64.0.10", // 管理コマンドの内線は既定の PBX
		"password=bob-201",
		"set_var=MYVOIP_EXT=" + b[0].Number,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("relay config missing %q", want)
		}
	}
	// PBX のアドレスを変えると登録先も変わる
	bob.setPBX("100.64.0.23")
	if cfg := h.relayConfig(); !strings.Contains(cfg, "client_uri=sip:201@100.64.0.23") || strings.Contains(cfg, "100.64.0.22") {
		t.Fatal("relay config not updated for the new PBX")
	}
	// 画面には PBX での内線番号を出す
	if page := alice.do(http.MethodGet, "/", nil).Body.String(); !strings.Contains(page, "内線 201") || !strings.Contains(page, "100.64.0.21:5060") {
		t.Fatal("dashboard does not show the PBX and extension")
	}
}

func TestPhoneInputIsValidated(t *testing.T) {
	h := newHarness(t, nil)
	c := h.browser()
	c.login("alice")
	c.setPBX("100.64.0.21")
	for _, form := range []url.Values{
		{"pbx_extension": {"20 1"}, "password": {"pw"}},
		{"pbx_extension": {"201"}, "password": {"pw\n[evil]"}},
		{"pbx_extension": {"201"}, "password": {""}},
		{"pbx_extension": {"201"}, "sip_user": {"a;b"}, "password": {"pw"}},
	} {
		if rec := c.post("/phones", form); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v -> %d", form, rec.Code)
		}
	}
	if len(h.phones("alice")) != 0 {
		t.Fatal("invalid phone was saved")
	}
}

func TestUpdateSIPKeepsPasswordWhenEmpty(t *testing.T) {
	h := newHarness(t, nil)
	c := h.browser()
	c.login("alice")
	c.setPBX("100.64.0.21")
	c.addPhone("仕事用", "201", "old-pw")
	line := h.phones("alice")[0].Number
	if rec := c.post("/phones/"+line+"/sip", url.Values{"pbx_extension": {"301"}, "sip_user": {"301"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("update -> %d %s", rec.Code, rec.Body.String())
	}
	e := h.phones("alice")[0]
	if pw, _ := h.box.Open(e.SIPPasswordEnc); pw != "old-pw" || e.PBXExtension != "301" || e.SIPUsername != "301" || e.SIPAuthUsername != "301" {
		t.Fatalf("after update: %+v pw=%s", e, pw)
	}
	c.post("/phones/"+line+"/sip", url.Values{"pbx_extension": {"301"}, "password": {"new-pw"}})
	if pw, _ := h.box.Open(h.phones("alice")[0].SIPPasswordEnc); pw != "new-pw" {
		t.Fatal("password not changed")
	}
	if !strings.Contains(h.relayConfig(), "password=new-pw") {
		t.Fatal("relay config not updated")
	}
}

func TestCannotTouchOtherUsersPhone(t *testing.T) {
	h := newHarness(t, nil)
	alice, bob := h.browser(), h.browser()
	alice.login("alice")
	bob.login("bob")
	alice.setPBX("100.64.0.21")
	alice.addPhone("仕事用", "201", "pw")
	line := h.phones("alice")[0].Number
	for _, path := range []string{"/phones/" + line + "/code", "/phones/" + line + "/delete", "/phones/" + line + "/rename", "/phones/" + line + "/sip", "/phones/9002/code"} {
		if rec := bob.post(path, url.Values{"pbx_extension": {"999"}, "label": {"x"}}); rec.Code != http.StatusNotFound {
			t.Fatalf("%s by other user -> %d", path, rec.Code)
		}
	}
	if e := h.phones("alice"); len(e) != 1 || e[0].PBXExtension != "201" || e[0].Label != "仕事用" {
		t.Fatal("other user's action changed the phone")
	}
	// 管理者以外は管理画面を開けない
	if rec := bob.do(http.MethodGet, "/admin", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin /admin -> %d", rec.Code)
	}
}

func TestPostWithoutCSRFIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	c := h.browser()
	c.login("alice")
	if rec := c.do(http.MethodPost, "/pbx", url.Values{"host": {"100.64.0.21"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("without csrf -> %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/pbx", url.Values{"csrf": {"wrong"}, "host": {"100.64.0.21"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong csrf -> %d", rec.Code)
	}
	if h.userByDiscord("alice").PBXHost != "" {
		t.Fatal("saved without csrf")
	}
}

var (
	bigCodePattern    = regexp.MustCompile(`<code class="big">([0-9A-Z-]+)</code>`)
	enrollCodePattern = regexp.MustCompile(`class="copy-source code-input" readonly value="([0-9A-Z-]+)"`)
)

func TestEnrollmentCodeEnrollsAndDeleteRemoves(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.browser()
	c.login("alice")
	c.setPBX("100.64.0.21")
	c.addPhone("仕事用", "201", "pbx-pw")
	ext := h.phones("alice")[0]
	rec := c.post("/phones/"+ext.Number+"/code", nil)
	m := enrollCodePattern.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil || !strings.Contains(rec.Body.String(), "data:image/png;base64,") {
		t.Fatalf("code page -> %d", rec.Code)
	}

	// アプリの登録 API で使える。iPhone は中継用 Asterisk にサーバーの識別子で登録し、画面には PBX の内線番号を出す
	enroll := httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader(`{"code":"`+m[1]+`","device_name":"iPhone","push_environment":"production"}`))
	enroll.RemoteAddr = "127.0.0.1:50001"
	er := httptest.NewRecorder()
	h.api.ServeHTTP(er, enroll)
	var resp struct {
		Extension struct {
			Number string `json:"number"`
		} `json:"extension"`
		SIP struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"sip"`
	}
	if er.Code != http.StatusOK || json.Unmarshal(er.Body.Bytes(), &resp) != nil {
		t.Fatalf("enroll -> %d %s", er.Code, er.Body.String())
	}
	if resp.SIP.Username != ext.Number || resp.Extension.Number != "201" || resp.SIP.Password == "pbx-pw" {
		t.Fatalf("enroll response = %+v", resp)
	}
	if mobile, _ := h.box.Open(ext.MobilePasswordEnc); mobile != resp.SIP.Password {
		t.Fatal("enroll did not return the relay password")
	}
	device, err := h.st.ActiveDeviceForExtension(ctx, ext.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.do(http.MethodGet, "/", nil).Body.String(), "登録済み") {
		t.Fatal("dashboard does not show the enrolled phone")
	}
	_ = h.st.AddHistory(ctx, store.HistoryItem{ID: "h1", Extension: ext.Number, Direction: "in", Result: "answered", StartedAt: time.Now()})

	if rec := c.post("/phones/"+ext.Number+"/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete -> %d", rec.Code)
	}
	if _, err := h.st.GetExtension(ctx, ext.Number); err == nil {
		t.Fatal("extension not deleted")
	}
	if _, err := h.st.GetDevice(ctx, device.ID); err == nil {
		t.Fatal("device not deleted")
	}
	if items, _ := h.st.ListHistory(ctx, ext.Number, 0, 10); len(items) != 0 {
		t.Fatalf("history kept: %d", len(items))
	}
	if strings.Contains(h.relayConfig(), ext.Number) {
		t.Fatal("deleted phone is still in the relay config")
	}
}

func TestSupportPasscodeLetsAdminOperate(t *testing.T) {
	h := newHarness(t, nil)
	alice, admin := h.browser(), h.browser()
	alice.login("alice")
	admin.login("admin")

	rec := alice.post("/support-passcode", nil)
	m := bigCodePattern.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("passcode page -> %d", rec.Code)
	}
	passcode := m[1]

	// サポート開始前は /support を開けない
	if rec := admin.do(http.MethodGet, "/support", nil); rec.Header().Get("Location") != "/admin" {
		t.Fatalf("/support before start -> %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := admin.post("/admin/support", url.Values{"passcode": {passcode}}); rec.Header().Get("Location") != "/support" {
		t.Fatalf("start support -> %d %s", rec.Code, rec.Body.String())
	}
	if rec := admin.do(http.MethodGet, "/support", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Name alice") {
		t.Fatalf("support dashboard -> %d", rec.Code)
	}
	// 管理者が利用者の PBX と iPhone を設定する
	if rec := admin.post("/support/pbx", url.Values{"host": {"100.64.0.21"}}); rec.Header().Get("Location") != "/support?m=pbx_saved" {
		t.Fatalf("support pbx -> %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := admin.post("/support/phones", url.Values{"label": {"サポートで追加"}, "pbx_extension": {"201"}, "password": {"pw"}}); rec.Header().Get("Location") != "/support?m=added" {
		t.Fatalf("support add -> %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if owned := h.phones("alice"); len(owned) != 1 || owned[0].Label != "サポートで追加" {
		t.Fatalf("support add did not create alice's phone: %+v", owned)
	}
	if h.userByDiscord("admin").PBXHost != "" {
		t.Fatal("support changed the admin's own PBX")
	}
	// 本人の画面に管理者の操作が記録される
	page := alice.do(http.MethodGet, "/", nil).Body.String()
	for _, want := range []string{"管理者がサポートを開始", "管理者（Name admin）", "iPhone を追加", "PBX の設定を変更"} {
		if !strings.Contains(page, want) {
			t.Fatalf("audit missing %q", want)
		}
	}
	// パスコードは1回限り
	other := h.browser()
	other.login("admin")
	if rec := other.post("/admin/support", url.Values{"passcode": {passcode}}); rec.Code != http.StatusForbidden {
		t.Fatalf("reused passcode -> %d", rec.Code)
	}
	if rec := admin.post("/support/end", nil); rec.Header().Get("Location") != "/admin?m=support_end" {
		t.Fatalf("end support -> %d", rec.Code)
	}
	if rec := admin.post("/support/phones", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("support action after end -> %d", rec.Code)
	}
}

func TestSupportPasscodeAttemptsAreLimited(t *testing.T) {
	h := newHarness(t, nil)
	alice, admin := h.browser(), h.browser()
	alice.login("alice")
	admin.login("admin")
	m := bigCodePattern.FindStringSubmatch(alice.post("/support-passcode", nil).Body.String())
	for i := 0; i < 5; i++ {
		if rec := admin.post("/admin/support", url.Values{"passcode": {"0000-0000"}}); rec.Code != http.StatusForbidden {
			t.Fatalf("wrong passcode %d -> %d", i, rec.Code)
		}
	}
	if rec := admin.post("/admin/support", url.Values{"passcode": {m[1]}}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 5 failures -> %d", rec.Code)
	}
}

func TestAdminAssignsExistingExtension(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	alice, admin := h.browser(), h.browser()
	alice.login("alice")
	admin.login("admin")
	aliceID := h.userByDiscord("alice").ID
	if rec := admin.post("/admin/extensions/9002/assign", url.Values{"user_id": {aliceID}, "label": {"既存"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("assign -> %d %s", rec.Code, rec.Body.String())
	}
	if e, _ := h.st.GetExtension(ctx, "9002"); e.OwnerUserID != aliceID || e.Label != "既存" {
		t.Fatalf("9002 = %+v", e)
	}
	if rec := admin.do(http.MethodGet, "/admin", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Name alice・既存") {
		t.Fatalf("admin page -> %d", rec.Code)
	}
	// 利用者が PBX を設定すると、割り当てた内線もその PBX へ登録する
	alice.setPBX("100.64.0.21")
	if !strings.Contains(h.relayConfig(), "client_uri=sip:9002@100.64.0.21") {
		t.Fatal("assigned extension not moved to the user's PBX")
	}
	// 利用者が削除すると、管理コマンドの内線は消さずに割り当てだけを外す
	if rec := alice.post("/phones/9002/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete -> %d", rec.Code)
	}
	e, err := h.st.GetExtension(ctx, "9002")
	if err != nil || e.OwnerUserID != "" {
		t.Fatalf("9002 after delete = %+v %v", e, err)
	}
	if !strings.Contains(h.relayConfig(), "client_uri=sip:9002@100.64.0.10") {
		t.Fatal("released extension not back on the default PBX")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.browser().do(http.MethodGet, "/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/login -> %d", rec.Code)
	}
	for _, header := range []string{"Content-Security-Policy", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(header) == "" {
			t.Fatalf("missing %s", header)
		}
	}
	if rec := h.browser().do(http.MethodGet, "/static/app.css", nil); rec.Code != http.StatusOK {
		t.Fatalf("/static/app.css -> %d", rec.Code)
	}
}

// --- 保留音

// sineWAV は 8kHz・モノラル・16bit の正弦波の WAV を作る。
func sineWAV(seconds float64) []byte {
	n := int(8000 * seconds)
	b := make([]byte, 44+n*2)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+n*2))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 8000)
	binary.LittleEndian.PutUint32(b[28:], 16000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n*2))
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/8000))
		binary.LittleEndian.PutUint16(b[44+i*2:], uint16(v))
	}
	return b
}

func (c *client) upload(path, name, filename string, data []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("csrf", c.csrf)
	_ = mw.WriteField("name", name)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.send(req)
}

func (h *harness) holdMusic(discordID string) []store.MOHFile {
	h.t.Helper()
	list, err := h.st.ListMOHByOwner(context.Background(), h.userByDiscord(discordID).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func TestHoldMusicIsPrivateToItsOwner(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	alice, bob := h.browser(), h.browser()
	alice.login("alice")
	bob.login("bob")
	alice.setPBX("100.64.0.21")
	bob.setPBX("100.64.0.22")
	alice.addPhone("仕事用", "201", "pw")
	bob.addPhone("bob", "201", "pw")
	if rec := alice.upload("/moh", "アリスの曲", "song.wav", sineWAV(3)); rec.Header().Get("Location") != "/?m=moh_added" {
		t.Fatalf("upload -> %d %s", rec.Code, rec.Body.String())
	}
	moh := h.holdMusic("alice")
	if len(moh) != 1 || moh[0].Name != "アリスの曲" || moh[0].DurationMS < 2900 || moh[0].DurationMS > 3100 {
		t.Fatalf("moh = %+v", moh)
	}
	id := moh[0].ID
	aliceLine, bobLine := h.phones("alice")[0].Number, h.phones("bob")[0].Number

	// 本人は試聴できる（範囲指定の取得にも応じる）
	rec := alice.do(http.MethodGet, "/moh/"+id+"/audio", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/wav" || !bytes.HasPrefix(rec.Body.Bytes(), []byte("RIFF")) {
		t.Fatalf("owner audio -> %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	req := httptest.NewRequest(http.MethodGet, "/moh/"+id+"/audio", nil)
	req.Header.Set("Range", "bytes=0-99")
	if rec := alice.send(req); rec.Code != http.StatusPartialContent || rec.Body.Len() != 100 {
		t.Fatalf("range -> %d len %d", rec.Code, rec.Body.Len())
	}
	// 他人は試聴・削除・割り当てのどれもできず、存在するかも分からない（404）
	if rec := bob.do(http.MethodGet, "/moh/"+id+"/audio", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other user's audio -> %d", rec.Code)
	}
	if rec := bob.post("/moh/"+id+"/delete", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other user's delete -> %d", rec.Code)
	}
	if rec := bob.post("/phones/"+bobLine+"/moh", url.Values{"moh_id": {id}}); rec.Code != http.StatusNotFound {
		t.Fatalf("assign other user's moh -> %d", rec.Code)
	}
	if rec := bob.post("/phones/"+aliceLine+"/moh", url.Values{"moh_id": {""}}); rec.Code != http.StatusNotFound {
		t.Fatalf("change other user's phone -> %d", rec.Code)
	}
	if e, _ := h.st.GetExtension(ctx, bobLine); e.MOHFileID != "" {
		t.Fatal("bob's phone got alice's hold music")
	}
	if page := bob.do(http.MethodGet, "/", nil).Body.String(); strings.Contains(page, "アリスの曲") || strings.Contains(page, id) {
		t.Fatal("bob's page shows alice's hold music")
	}
	if len(h.holdMusic("alice")) != 1 {
		t.Fatal("other user deleted the hold music")
	}

	// 本人は自分の iPhone に割り当てられ、中継用 Asterisk に書き出される
	if rec := alice.post("/phones/"+aliceLine+"/moh", url.Values{"moh_id": {id}}); rec.Header().Get("Location") != "/?m=moh_saved" {
		t.Fatalf("assign -> %d %s", rec.Code, rec.Body.String())
	}
	cfg := h.relayConfig()
	if !strings.Contains(cfg, "moh_suggest="+relay.MOHClass(id)) || strings.Count(cfg, "moh_suggest=") != 1 {
		t.Fatalf("relay config:\n%s", cfg)
	}
	if _, err := os.Stat(filepath.Join(h.relayDir, "moh", id, "hold.wav")); err != nil {
		t.Fatal(err)
	}
	if page := alice.do(http.MethodGet, "/", nil).Body.String(); !strings.Contains(page, "保留音：アリスの曲") || !strings.Contains(page, `<audio controls preload="none" src="/moh/`+id+`/audio">`) {
		t.Fatal("dashboard does not show the assigned hold music")
	}

	// 削除すると、割り当てた iPhone は標準の保留音に戻る
	if rec := alice.post("/moh/"+id+"/delete", nil); rec.Header().Get("Location") != "/?m=moh_deleted" {
		t.Fatalf("delete -> %d", rec.Code)
	}
	if e, _ := h.st.GetExtension(ctx, aliceLine); e.MOHFileID != "" {
		t.Fatal("assignment remains after delete")
	}
	if strings.Contains(h.relayConfig(), "moh_suggest") {
		t.Fatal("deleted hold music still configured")
	}
	if _, err := os.Stat(filepath.Join(h.relayDir, "moh", id)); !os.IsNotExist(err) {
		t.Fatal("deleted hold music file remains")
	}
}

func TestSupportCannotListenToHoldMusic(t *testing.T) {
	h := newHarness(t, nil)
	alice, admin := h.browser(), h.browser()
	alice.login("alice")
	admin.login("admin")
	alice.setPBX("100.64.0.21")
	alice.addPhone("仕事用", "201", "pw")
	alice.upload("/moh", "曲", "a.wav", sineWAV(2))
	id := h.holdMusic("alice")[0].ID
	line := h.phones("alice")[0].Number
	m := bigCodePattern.FindStringSubmatch(alice.post("/support-passcode", nil).Body.String())
	admin.post("/admin/support", url.Values{"passcode": {m[1]}})

	page := admin.do(http.MethodGet, "/support", nil).Body.String()
	if !strings.Contains(page, "曲") || strings.Contains(page, "<audio") {
		t.Fatal("support page must list hold music without a player")
	}
	if rec := admin.do(http.MethodGet, "/moh/"+id+"/audio", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("admin audio in support -> %d", rec.Code)
	}
	// サポートでは割り当て・追加・削除はできる
	if rec := admin.post("/support/phones/"+line+"/moh", url.Values{"moh_id": {id}}); rec.Header().Get("Location") != "/support?m=moh_saved" {
		t.Fatalf("support assign -> %d", rec.Code)
	}
	if rec := admin.upload("/support/moh", "管理者が追加", "b.wav", sineWAV(2)); rec.Header().Get("Location") != "/support?m=moh_added" {
		t.Fatalf("support upload -> %d %s", rec.Code, rec.Body.String())
	}
	if list := h.holdMusic("alice"); len(list) != 2 || len(h.holdMusic("admin")) != 0 {
		t.Fatal("support upload went to the wrong user")
	}
	if !strings.Contains(alice.do(http.MethodGet, "/", nil).Body.String(), "保留音を変更") {
		t.Fatal("support action not in the audit log")
	}
}

func TestHoldMusicUploadIsValidated(t *testing.T) {
	h := newHarness(t, nil)
	c := h.browser()
	c.login("alice")
	for name, data := range map[string][]byte{
		"text":      []byte("not audio"),
		"too long":  sineWAV(31),
		"too short": sineWAV(0.5),
	} {
		if rec := c.upload("/moh", name, "x.wav", data); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s -> %d", name, rec.Code)
		}
	}
	c.upload("/moh", "1", "1.wav", sineWAV(1.5))
	c.upload("/moh", "2", "2.wav", sineWAV(1.5))
	if rec := c.upload("/moh", "3", "3.wav", sineWAV(1.5)); rec.Code != http.StatusConflict {
		t.Fatalf("over limit -> %d", rec.Code)
	}
	// 名前を省略するとファイル名を使う。名前の制御文字は除く
	h2 := newHarness(t, nil)
	d := h2.browser()
	d.login("alice")
	d.upload("/moh", "", "ロビー.wav", sineWAV(1.5))
	d.upload("/moh", "受付\x07の曲", "x.mp3", sineWAV(1.5))
	if list := h2.holdMusic("alice"); len(list) != 2 || list[0].Name != "ロビー" || list[1].Name != "受付の曲" {
		t.Fatalf("names = %+v", list)
	}
	// CSRF トークンがなければ受け付けない
	d.csrf = "wrong"
	if rec := d.upload("/moh", "x", "x.wav", sineWAV(1.5)); rec.Code != http.StatusForbidden {
		t.Fatalf("upload without csrf -> %d", rec.Code)
	}
}
