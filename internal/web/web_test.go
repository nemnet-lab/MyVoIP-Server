package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nemnet-lab/MyVoIP-Server/internal/api"
	"github.com/nemnet-lab/MyVoIP-Server/internal/config"
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
	h := &harness{t: t, st: store.NewMemory(), box: box}
	ctx := context.Background()
	for _, n := range []string{"9101", "9102", "9103", "9002"} {
		if err := h.st.UpsertExtension(ctx, store.Extension{
			Number: n, SIPUsername: n, SIPAuthUsername: n, SIPPasswordEnc: box.Seal("pbx-" + n), Endpoint: n, RingTimeoutSeconds: 30,
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.st.SetMobilePassword(ctx, n, box.Seal("mobile-"+n)); err != nil {
			t.Fatal(err)
		}
		if n != "9002" {
			if err := h.st.SetExtensionPool(ctx, n, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(h.st, box, Config{
		PublicURL: "http://myvoip.test", EnrollmentCodeTTL: 10 * time.Minute, Relay: true,
		Web: config.WebConfig{
			Enabled: true, DiscordClientID: "client-id", DiscordClientSecret: "client-secret",
			DiscordGuildID: "guild-1", DiscordRoleIDs: roles, AdminIDs: []string{"admin"},
			MaxPhonesPerUser: 2, SessionTTL: time.Hour, SupportCodeTTL: 30 * time.Minute, SupportSessionTTL: time.Hour,
		},
		DiscordAPI: ds.URL, DiscordAuthorizeURL: "https://discord.example/oauth2/authorize",
	}, func(context.Context) error { h.syncs++; return nil }, log)
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
	req.RemoteAddr = "127.0.0.1:50000"
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
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

func TestAddPhoneAssignsLowestFreeExtensionWithinLimit(t *testing.T) {
	h := newHarness(t, nil)
	alice, bob := h.browser(), h.browser()
	alice.login("alice")
	bob.login("bob")
	for _, label := range []string{"仕事用", "私用"} {
		if rec := alice.post("/phones", url.Values{"label": {label}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("add -> %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := alice.post("/phones", nil); rec.Code != http.StatusConflict {
		t.Fatalf("over limit -> %d", rec.Code)
	}
	owned, _ := h.st.ListExtensionsByOwner(context.Background(), h.userByDiscord("alice").ID)
	if len(owned) != 2 || owned[0].Number != "9101" || owned[1].Number != "9102" || owned[0].Label != "仕事用" {
		t.Fatalf("owned = %+v", owned)
	}
	if rec := bob.post("/phones", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("bob add -> %d", rec.Code)
	}
	if e, _ := h.st.GetExtension(context.Background(), "9103"); e.OwnerUserID != h.userByDiscord("bob").ID || e.Label != "iPhone" {
		t.Fatalf("9103 = %+v", e)
	}
	// 割り当て用でない 9002 は使わない
	if rec := bob.post("/phones", nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "残っていません") {
		t.Fatalf("no free -> %d", rec.Code)
	}
	if h.syncs != 3 {
		t.Fatalf("relay syncs = %d", h.syncs)
	}
}

func TestCannotTouchOtherUsersPhone(t *testing.T) {
	h := newHarness(t, nil)
	alice, bob := h.browser(), h.browser()
	alice.login("alice")
	bob.login("bob")
	alice.post("/phones", nil)
	for _, path := range []string{"/phones/9101/code", "/phones/9101/delete", "/phones/9101/rename", "/phones/9002/code"} {
		if rec := bob.post(path, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s by other user -> %d", path, rec.Code)
		}
	}
	if e, _ := h.st.GetExtension(context.Background(), "9101"); e.OwnerUserID != h.userByDiscord("alice").ID {
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
	if rec := c.do(http.MethodPost, "/phones", url.Values{"label": {"x"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("without csrf -> %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/phones", url.Values{"csrf": {"wrong"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong csrf -> %d", rec.Code)
	}
	if owned, _ := h.st.ListExtensionsByOwner(context.Background(), h.userByDiscord("alice").ID); len(owned) != 0 {
		t.Fatal("phone added without csrf")
	}
}

var (
	bigCodePattern    = regexp.MustCompile(`<code class="big">([0-9A-Z-]+)</code>`)
	enrollCodePattern = regexp.MustCompile(`class="copy-source code-input" readonly value="([0-9A-Z-]+)"`)
)

func TestEnrollmentCodeEnrollsAndDeleteRevokes(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.browser()
	c.login("alice")
	c.post("/phones", url.Values{"label": {"仕事用"}})
	rec := c.post("/phones/9101/code", nil)
	m := enrollCodePattern.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil || !strings.Contains(rec.Body.String(), "data:image/png;base64,") {
		t.Fatalf("code page -> %d", rec.Code)
	}

	// アプリの登録 API で使える。中継モードなので中継用のパスワードが渡る
	enroll := httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader(`{"code":"`+m[1]+`","device_name":"iPhone","push_environment":"production"}`))
	enroll.RemoteAddr = "127.0.0.1:50001"
	er := httptest.NewRecorder()
	h.api.ServeHTTP(er, enroll)
	var resp struct {
		SIP struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"sip"`
	}
	if er.Code != http.StatusOK || json.Unmarshal(er.Body.Bytes(), &resp) != nil {
		t.Fatalf("enroll -> %d %s", er.Code, er.Body.String())
	}
	before, _ := h.st.GetExtension(ctx, "9101")
	if resp.SIP.Username != "9101" || resp.SIP.Password == "" {
		t.Fatalf("sip = %+v", resp.SIP)
	}
	if mobile, _ := h.box.Open(before.MobilePasswordEnc); mobile != resp.SIP.Password {
		t.Fatal("enroll did not return the relay password")
	}
	device, err := h.st.ActiveDeviceForExtension(ctx, "9101")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.do(http.MethodGet, "/", nil).Body.String(), "登録済み") {
		t.Fatal("dashboard does not show the enrolled phone")
	}
	_ = h.st.AddHistory(ctx, store.HistoryItem{ID: "h1", Extension: "9101", Direction: "in", Result: "answered", StartedAt: time.Now()})

	if rec := c.post("/phones/9101/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete -> %d", rec.Code)
	}
	if d, _ := h.st.GetDevice(ctx, device.ID); d.RevokedAt == nil {
		t.Fatal("device not revoked")
	}
	after, _ := h.st.GetExtension(ctx, "9101")
	if after.OwnerUserID != "" || after.Label != "" {
		t.Fatalf("extension still assigned: %+v", after)
	}
	if mobile, _ := h.box.Open(after.MobilePasswordEnc); mobile == resp.SIP.Password {
		t.Fatal("relay password not rotated")
	}
	if items, _ := h.st.ListHistory(ctx, "9101", 0, 10); len(items) != 0 {
		t.Fatalf("history kept for the next user: %d", len(items))
	}
	// 次の利用者に割り当てても、前の利用者の端末・履歴は引き継がない
	bob := h.browser()
	bob.login("bob")
	bob.post("/phones", nil)
	if e, _ := h.st.GetExtension(ctx, "9101"); e.OwnerUserID != h.userByDiscord("bob").ID {
		t.Fatalf("released extension not reused: %+v", e)
	}
	if _, err := h.st.ActiveDeviceForExtension(ctx, "9101"); err == nil {
		t.Fatal("previous user's device is still active")
	}
}

func TestSupportPasscodeLetsAdminOperate(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
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
	if rec := admin.post("/support/phones", url.Values{"label": {"サポートで追加"}}); rec.Header().Get("Location") != "/support?m=added" {
		t.Fatalf("support add -> %d %q", rec.Code, rec.Header().Get("Location"))
	}
	aliceUser := h.userByDiscord("alice")
	owned, _ := h.st.ListExtensionsByOwner(ctx, aliceUser.ID)
	if len(owned) != 1 || owned[0].Label != "サポートで追加" {
		t.Fatalf("support add did not create alice's phone: %+v", owned)
	}
	// 本人の画面に管理者の操作が記録される
	page := alice.do(http.MethodGet, "/", nil).Body.String()
	for _, want := range []string{"管理者がサポートを開始", "管理者（Name admin）", "iPhone を追加"} {
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
	alice, admin := h.browser(), h.browser()
	alice.login("alice")
	admin.login("admin")
	aliceID := h.userByDiscord("alice").ID
	if rec := admin.post("/admin/extensions/9002/assign", url.Values{"user_id": {aliceID}, "label": {"既存"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("assign -> %d %s", rec.Code, rec.Body.String())
	}
	if e, _ := h.st.GetExtension(context.Background(), "9002"); e.OwnerUserID != aliceID || e.Label != "既存" {
		t.Fatalf("9002 = %+v", e)
	}
	if rec := admin.do(http.MethodGet, "/admin", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Name alice・既存") {
		t.Fatalf("admin page -> %d", rec.Code)
	}
	// 管理者が内線を追加する（割り当て用）
	if rec := admin.post("/admin/extensions", url.Values{"number": {"9104"}, "password": {"pw"}, "pool": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save extension -> %d %s", rec.Code, rec.Body.String())
	}
	e, err := h.st.GetExtension(context.Background(), "9104")
	if err != nil || !e.Pool || e.SIPUsername != "9104" {
		t.Fatalf("9104 = %+v %v", e, err)
	}
	if pw, _ := h.box.Open(e.SIPPasswordEnc); pw != "pw" {
		t.Fatal("password not stored")
	}
	if rec := admin.post("/admin/extensions", url.Values{"number": {"91;04"}, "password": {"pw"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid number -> %d", rec.Code)
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
