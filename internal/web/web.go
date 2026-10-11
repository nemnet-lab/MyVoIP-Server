// Package web は Discord でログインする Web 管理画面。
//
// 利用者は自分の iPhone を作成し（pool の空き内線を1台に1つ割り当てる）、アプリに入力する登録コードを発行する。
// 管理者のサポートが必要なときは、利用者がサポート用パスコードを発行して管理者に伝える。
// 管理者はそのパスコードで、期限付きで利用者の画面を開いて操作できる（操作は利用者の画面に記録される）。
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/nemnet-lab/MyVoIP-Server/internal/config"
	"github.com/nemnet-lab/MyVoIP-Server/internal/relay"
	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	sessionCookie = "myvoip_session"
	stateCookie   = "myvoip_oauth_state"
)

type Config struct {
	PublicURL         string
	EnrollmentCodeTTL time.Duration
	// Relay は中継モード（iPhone を削除したら中継用のパスワードを作り直し、設定を反映する）
	Relay bool
	Web   config.WebConfig
	// DiscordAPI と DiscordAuthorizeURL は試験で差し替える（空なら Discord 本番）
	DiscordAPI          string
	DiscordAuthorizeURL string
}

// Syncer は中継用 Asterisk に内線の設定を反映する（中継モード以外は nil）。
type Syncer func(ctx context.Context) error

type Server struct {
	store store.Store
	box   *secret.Box
	cfg   Config
	sync  Syncer
	log   *slog.Logger
	http  *http.Client
	pages map[string]*template.Template

	syncMu sync.Mutex
	// supportFails は管理者ごとのパスコード入力の失敗（総当たり対策）
	failMu       sync.Mutex
	supportFails map[string][]time.Time
}

func New(st store.Store, box *secret.Box, cfg Config, syncer Syncer, log *slog.Logger) (*Server, error) {
	if cfg.DiscordAPI == "" {
		cfg.DiscordAPI = "https://discord.com/api/v10"
	}
	if cfg.DiscordAuthorizeURL == "" {
		cfg.DiscordAuthorizeURL = "https://discord.com/oauth2/authorize"
	}
	s := &Server{
		store: st, box: box, cfg: cfg, sync: syncer, log: log,
		http:         &http.Client{Timeout: 10 * time.Second},
		pages:        map[string]*template.Template{},
		supportFails: map[string][]time.Time{},
	}
	funcs := template.FuncMap{
		"datetime": func(t time.Time) string { return t.Local().Format("2006/01/02 15:04") },
		"clock":    func(t time.Time) string { return t.Local().Format("15:04") },
	}
	for _, name := range []string{"login", "dashboard", "code", "passcode", "admin", "enroll", "message"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		s.pages[name] = t
	}
	return s, nil
}

func (s *Server) secureCookies() bool { return strings.HasPrefix(s.cfg.PublicURL, "https://") }

func (s *Server) isAdminID(discordID string) bool { return contains(s.cfg.Web.AdminIDs, discordID) }

// Handler は Web 管理画面の画面を返す。API（/v1/）とは別の経路で、同じ待受ポートに載せる。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", s.withUser(s.dashboard))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /enroll", s.enrollInfo)
	mux.HandleFunc("GET /auth/discord", s.startLogin)
	mux.HandleFunc("GET /auth/discord/callback", s.callback)
	mux.HandleFunc("POST /logout", s.withUser(s.logout))

	mux.HandleFunc("POST /phones", s.withUser(s.self(s.addPhone)))
	mux.HandleFunc("POST /phones/{number}/code", s.withUser(s.self(s.issueCode)))
	mux.HandleFunc("POST /phones/{number}/rename", s.withUser(s.self(s.renamePhone)))
	mux.HandleFunc("POST /phones/{number}/delete", s.withUser(s.self(s.deletePhone)))
	mux.HandleFunc("POST /support-passcode", s.withUser(s.createPasscode))
	mux.HandleFunc("POST /support-passcode/delete", s.withUser(s.deletePasscode))

	mux.HandleFunc("GET /admin", s.withUser(s.adminOnly(s.adminPage)))
	mux.HandleFunc("POST /admin/support", s.withUser(s.adminOnly(s.startSupport)))
	mux.HandleFunc("POST /admin/extensions", s.withUser(s.adminOnly(s.saveExtension)))
	mux.HandleFunc("POST /admin/extensions/{number}/pool", s.withUser(s.adminOnly(s.setPool)))
	mux.HandleFunc("POST /admin/extensions/{number}/assign", s.withUser(s.adminOnly(s.assignExtension)))
	mux.HandleFunc("POST /admin/extensions/{number}/release", s.withUser(s.adminOnly(s.releaseExtension)))
	mux.HandleFunc("POST /admin/users/{id}/disabled", s.withUser(s.adminOnly(s.setUserDisabled)))

	mux.HandleFunc("GET /support", s.withUser(s.adminOnly(s.supportDashboard)))
	mux.HandleFunc("POST /support/end", s.withUser(s.adminOnly(s.endSupport)))
	mux.HandleFunc("POST /support/phones", s.withUser(s.adminOnly(s.support(s.addPhone))))
	mux.HandleFunc("POST /support/phones/{number}/code", s.withUser(s.adminOnly(s.support(s.issueCode))))
	mux.HandleFunc("POST /support/phones/{number}/rename", s.withUser(s.adminOnly(s.support(s.renamePhone))))
	mux.HandleFunc("POST /support/phones/{number}/delete", s.withUser(s.adminOnly(s.support(s.deletePhone))))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// Discord から戻る URL の認可コードを外部に送らない
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		next.ServeHTTP(w, r)
	})
}

// --- 画面の描画

type page struct {
	Title   string
	User    *store.User
	IsAdmin bool
	CSRF    string
	// Support はサポート中の利用者（管理者が開いているとき）
	Support      *store.User
	SupportUntil time.Time
	Notice       string
	Error        string
	Data         any
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		s.log.Error("web: render", "page", name, "error", err)
	}
}

func (s *Server) message(w http.ResponseWriter, status int, rc *reqCtx, title, text string) {
	p := page{Title: title, Error: text}
	if rc != nil {
		p = rc.page(title)
		p.Error = text
	}
	s.render(w, status, "message", p)
}

// notices は画面上部に出すお知らせ（任意の文字列を URL から表示しないよう、決まった文だけ）。
var notices = map[string]string{
	"added":           "iPhone を追加しました。「登録コードを発行」から、アプリに入力するコードを表示できます。",
	"deleted":         "iPhone を削除しました。その iPhone のアプリは使えなくなります。",
	"renamed":         "名前を変更しました。",
	"passcode_delete": "サポート用パスコードを取り消しました。",
	"support_end":     "サポートを終了しました。",
	"saved":           "保存しました。",
	"sync_failed":     "保存しましたが、中継用 Asterisk への反映に失敗しました。しばらくしてからもう一度操作するか、relay sync を実行してください。",
}

func noticeFrom(r *http.Request) string { return notices[r.URL.Query().Get("m")] }

// --- ログイン状態

type reqCtx struct {
	sess  store.WebSession
	user  store.User
	admin bool
	// support はサポート中の利用者（期限内のときだけ）
	support *store.User
}

func (rc *reqCtx) page(title string) page {
	p := page{Title: title, User: &rc.user, IsAdmin: rc.admin, CSRF: rc.sess.CSRF, Support: rc.support}
	if rc.support != nil && rc.sess.SupportExpiresAt != nil {
		p.SupportUntil = *rc.sess.SupportExpiresAt
	}
	return p
}

type handler func(w http.ResponseWriter, r *http.Request, rc *reqCtx)

func (s *Server) loadSession(r *http.Request) (*reqCtx, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	ctx := r.Context()
	sess, err := s.store.GetWebSession(ctx, secret.Hash(c.Value))
	if err != nil || time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	user, err := s.store.GetUser(ctx, sess.UserID)
	if err != nil || user.Disabled {
		return nil, false
	}
	rc := &reqCtx{sess: sess, user: user, admin: s.isAdminID(user.DiscordID)}
	if rc.admin && sess.SupportUserID != "" && sess.SupportExpiresAt != nil && time.Now().Before(*sess.SupportExpiresAt) {
		if target, err := s.store.GetUser(ctx, sess.SupportUserID); err == nil {
			rc.support = &target
		}
	}
	return rc, true
}

// withUser はログインを求め、POST では CSRF トークンを確かめる。
func (s *Server) withUser(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc, ok := s.loadSession(r)
		if !ok {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			s.message(w, http.StatusForbidden, nil, "ログインが必要です", "ログインの有効期限が切れました。もう一度ログインしてください。")
			return
		}
		if r.Method == http.MethodPost {
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(rc.sess.CSRF)) != 1 {
				s.message(w, http.StatusForbidden, rc, "操作できません", "画面の有効期限が切れました。画面を開き直してから、もう一度操作してください。")
				return
			}
		}
		next(w, r, rc)
	}
}

func (s *Server) adminOnly(next handler) handler {
	return func(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
		if !rc.admin {
			http.NotFound(w, r)
			return
		}
		next(w, r, rc)
	}
}

// phoneHandler は「誰の iPhone を操作するか（subject）」と、戻り先を受け取る。
type phoneHandler func(w http.ResponseWriter, r *http.Request, rc *reqCtx, subject store.User, base string)

func (s *Server) self(next phoneHandler) handler {
	return func(w http.ResponseWriter, r *http.Request, rc *reqCtx) { next(w, r, rc, rc.user, "") }
}

func (s *Server) support(next phoneHandler) handler {
	return func(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
		if rc.support == nil {
			s.message(w, http.StatusForbidden, rc, "サポートの期限切れ", "サポートの期限が切れました。利用者に新しいサポート用パスコードを発行してもらってください。")
			return
		}
		next(w, r, rc, *rc.support, "/support")
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.loadSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p := page{Title: "ログイン"}
	switch r.URL.Query().Get("e") {
	case "denied":
		p.Error = "このアカウントではログインできません。管理者に、Discord サーバーへの参加やロールの付与を依頼してください。"
	case "disabled":
		p.Error = "このアカウントは管理者によって停止されています。"
	case "cancelled":
		p.Error = "Discord でのログインが取り消されました。"
	case "failed":
		p.Error = "Discord でのログインに失敗しました。もう一度お試しください。"
	}
	s.render(w, http.StatusOK, "login", p)
}

func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) {
	state := secret.NewToken()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/auth/discord", MaxAge: 600,
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.authorizeURL(state), http.StatusSeeOther)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/auth/discord", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode})
	if q.Get("error") != "" {
		http.Redirect(w, r, "/login?e=cancelled", http.StatusSeeOther)
		return
	}
	c, err := r.Cookie(stateCookie)
	if err != nil || c.Value == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(q.Get("state"))) != 1 || q.Get("code") == "" {
		http.Redirect(w, r, "/login?e=failed", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	token, err := s.exchangeCode(ctx, q.Get("code"))
	if err != nil {
		s.log.Warn("web: discord token exchange failed", "error", err)
		http.Redirect(w, r, "/login?e=failed", http.StatusSeeOther)
		return
	}
	var du discordUser
	if err := s.discordGet(ctx, token, "/users/@me", &du); err != nil || du.ID == "" {
		s.log.Warn("web: discord user lookup failed", "error", err)
		http.Redirect(w, r, "/login?e=failed", http.StatusSeeOther)
		return
	}
	if err := s.authorize(ctx, token, du); err != nil {
		if !errors.Is(err, errNotMember) {
			s.log.Warn("web: discord membership check failed", "error", err)
			http.Redirect(w, r, "/login?e=failed", http.StatusSeeOther)
			return
		}
		s.log.Info("web: login denied", "discord_id", du.ID)
		http.Redirect(w, r, "/login?e=denied", http.StatusSeeOther)
		return
	}
	user, err := s.store.UpsertDiscordUser(ctx, store.User{
		ID: "u-" + secret.NewID(), DiscordID: du.ID, DiscordUsername: du.Username, DisplayName: du.displayName(),
	}, time.Now())
	if err != nil {
		s.log.Error("web: save user", "error", err)
		http.Redirect(w, r, "/login?e=failed", http.StatusSeeOther)
		return
	}
	if user.Disabled {
		http.Redirect(w, r, "/login?e=disabled", http.StatusSeeOther)
		return
	}
	raw := secret.NewToken()
	if err := s.store.CreateWebSession(ctx, store.WebSession{
		Hash: secret.Hash(raw), UserID: user.ID, CSRF: secret.NewToken(), ExpiresAt: time.Now().Add(s.cfg.Web.SessionTTL),
	}); err != nil {
		s.log.Error("web: create session", "error", err)
		http.Redirect(w, r, "/login?e=failed", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: raw, Path: "/", MaxAge: int(s.cfg.Web.SessionTTL.Seconds()),
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	s.log.Info("web: login", "user_id", user.ID, "discord_id", du.ID, "admin", s.isAdminID(du.ID))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	_ = s.store.DeleteWebSession(r.Context(), rc.sess.Hash)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// enrollInfo は QR のリンクをブラウザで開いたときの案内。
func (s *Server) enrollInfo(w http.ResponseWriter, _ *http.Request) {
	s.render(w, http.StatusOK, "enroll", page{Title: "MyVoIP の登録"})
}

// --- 利用者の画面

type phoneView struct {
	Number      string
	Label       string
	DisplayName string
	Enrolled    bool
	DeviceName  string
	AppVersion  string
	Enabled     bool
	PushReady   bool
	EnrolledAt  time.Time
}

type auditView struct {
	At     time.Time
	Actor  string
	Action string
	Detail string
}

type dashboardData struct {
	Phones   []phoneView
	Max      int
	CanAdd   bool
	NoFree   bool
	Passcode *store.SupportPasscode
	Audit    []auditView
	// Base は操作の送り先（本人は ""、管理者のサポート中は "/support"）
	Base string
}

var actionLabels = map[string]string{
	"phone_added":      "iPhone を追加",
	"phone_deleted":    "iPhone を削除",
	"phone_renamed":    "iPhone の名前を変更",
	"code_issued":      "登録コードを発行",
	"passcode_issued":  "サポート用パスコードを発行",
	"passcode_deleted": "サポート用パスコードを取り消し",
	"support_started":  "管理者がサポートを開始",
	"support_ended":    "管理者がサポートを終了",
	"phone_assigned":   "管理者が内線を割り当て",
	"phone_released":   "管理者が iPhone を削除",
}

func (s *Server) dashboardFor(ctx context.Context, subject store.User, base string) (dashboardData, error) {
	exts, err := s.store.ListExtensionsByOwner(ctx, subject.ID)
	if err != nil {
		return dashboardData{}, err
	}
	data := dashboardData{Max: s.cfg.Web.MaxPhonesPerUser, Base: base}
	for _, e := range exts {
		v := phoneView{Number: e.Number, Label: e.Label, DisplayName: e.DisplayName}
		if d, err := s.store.ActiveDeviceForExtension(ctx, e.Number); err == nil {
			v.Enrolled, v.DeviceName, v.AppVersion = true, d.Name, d.AppVersion
			v.Enabled, v.PushReady, v.EnrolledAt = d.Enabled, d.VoIPToken != "", d.CreatedAt
		}
		data.Phones = append(data.Phones, v)
	}
	data.CanAdd = len(exts) < data.Max
	if data.CanAdd {
		all, err := s.store.ListExtensions(ctx)
		if err != nil {
			return dashboardData{}, err
		}
		data.NoFree = true
		for _, e := range all {
			if e.Pool && e.OwnerUserID == "" {
				data.NoFree = false
				break
			}
		}
	}
	if p, err := s.store.GetSupportPasscode(ctx, subject.ID); err == nil && time.Now().Before(p.ExpiresAt) {
		data.Passcode = &p
	}
	entries, err := s.store.ListAudit(ctx, subject.ID, 20)
	if err != nil {
		return dashboardData{}, err
	}
	names := map[string]string{subject.ID: "本人"}
	for _, a := range entries {
		actor, ok := names[a.ActorUserID]
		if !ok {
			actor = "管理者"
			if u, err := s.store.GetUser(ctx, a.ActorUserID); err == nil {
				actor = "管理者（" + u.DisplayName + "）"
			}
			names[a.ActorUserID] = actor
		}
		label := actionLabels[a.Action]
		if label == "" {
			label = a.Action
		}
		data.Audit = append(data.Audit, auditView{At: a.At, Actor: actor, Action: label, Detail: a.Detail})
	}
	return data, nil
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	data, err := s.dashboardFor(r.Context(), rc.user, "")
	if err != nil {
		s.log.Error("web: dashboard", "error", err)
		s.message(w, http.StatusInternalServerError, rc, "エラー", "画面を表示できませんでした。")
		return
	}
	p := rc.page("あなたの iPhone")
	p.Notice = noticeFrom(r)
	p.Data = data
	s.render(w, http.StatusOK, "dashboard", p)
}

func (s *Server) supportDashboard(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if rc.support == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	data, err := s.dashboardFor(r.Context(), *rc.support, "/support")
	if err != nil {
		s.log.Error("web: support dashboard", "error", err)
		s.message(w, http.StatusInternalServerError, rc, "エラー", "画面を表示できませんでした。")
		return
	}
	p := rc.page(rc.support.DisplayName + " さんの iPhone")
	p.Notice = noticeFrom(r)
	p.Data = data
	s.render(w, http.StatusOK, "dashboard", p)
}

func (s *Server) audit(ctx context.Context, userID, actorID, action, detail string) {
	if err := s.store.AddAudit(ctx, store.AuditEntry{At: time.Now(), UserID: userID, ActorUserID: actorID, Action: action, Detail: detail}); err != nil {
		s.log.Warn("web: audit", "error", err)
	}
}

// applyRelay は中継用 Asterisk に内線の設定を反映する。失敗しても DB の変更は残し、次の起動・操作で反映される。
func (s *Server) applyRelay(ctx context.Context) bool {
	if s.sync == nil {
		return true
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.sync(syncCtx); err != nil {
		s.log.Error("web: relay sync failed", "error", err)
		return false
	}
	return true
}

func backTo(base, notice string, ok bool) string {
	if base == "" {
		base = "/"
	}
	if !ok {
		notice = "sync_failed"
	}
	return base + "?m=" + notice
}

func cleanLabel(raw string) string {
	label := strings.TrimSpace(strings.Join(strings.Fields(raw), " "))
	if r := []rune(label); len(r) > 40 {
		label = string(r[:40])
	}
	if label == "" {
		label = "iPhone"
	}
	return label
}

// ownedExtension は subject の内線だけを返す（他人の内線は存在しないものとして扱う）。
func (s *Server) ownedExtension(ctx context.Context, subject store.User, number string) (store.Extension, bool) {
	e, err := s.store.GetExtension(ctx, number)
	if err != nil || e.OwnerUserID != subject.ID {
		return store.Extension{}, false
	}
	return e, true
}

// resetCredentials は内線の iPhone を失効させ、中継用のパスワードを作り直す（前の利用者の iPhone で登録できないように）。
func (s *Server) resetCredentials(ctx context.Context, number string) error {
	if d, err := s.store.ActiveDeviceForExtension(ctx, number); err == nil {
		if err := s.store.RevokeDevice(ctx, d.ID, time.Now()); err != nil {
			return err
		}
	}
	if s.cfg.Relay {
		return s.store.SetMobilePassword(ctx, number, s.box.Seal(relay.NewPassword()))
	}
	return nil
}

func (s *Server) addPhone(w http.ResponseWriter, r *http.Request, rc *reqCtx, subject store.User, base string) {
	ctx := r.Context()
	label := cleanLabel(r.PostFormValue("label"))
	e, err := s.store.AssignFreeExtension(ctx, subject.ID, label, s.cfg.Web.MaxPhonesPerUser, time.Now())
	switch {
	case errors.Is(err, store.ErrLimit):
		s.message(w, http.StatusConflict, rc, "追加できません", fmt.Sprintf("iPhone は %d 台まで作成できます。使わない iPhone を削除してから追加してください。", s.cfg.Web.MaxPhonesPerUser))
		return
	case errors.Is(err, store.ErrNoFreeExtension):
		s.message(w, http.StatusConflict, rc, "追加できません", "割り当てられる内線が残っていません。管理者に内線の追加を依頼してください。")
		return
	case err != nil:
		s.log.Error("web: assign extension", "error", err)
		s.message(w, http.StatusInternalServerError, rc, "エラー", "iPhone を追加できませんでした。")
		return
	}
	if err := s.resetCredentials(ctx, e.Number); err != nil {
		s.log.Error("web: reset credentials", "extension", e.Number, "error", err)
	}
	s.log.Info("web: phone added", "user_id", subject.ID, "extension", e.Number, "actor", rc.user.ID)
	s.audit(ctx, subject.ID, rc.user.ID, "phone_added", fmt.Sprintf("%s（内線 %s）", label, e.Number))
	ok := s.applyRelay(ctx)
	http.Redirect(w, r, backTo(base, "added", ok), http.StatusSeeOther)
}

type codeData struct {
	Phone     phoneView
	Code      string
	PublicURL string
	QR        template.URL
	ExpiresAt time.Time
	Base      string
}

func (s *Server) issueCode(w http.ResponseWriter, r *http.Request, rc *reqCtx, subject store.User, base string) {
	ctx := r.Context()
	e, ok := s.ownedExtension(ctx, subject, r.PathValue("number"))
	if !ok {
		s.message(w, http.StatusNotFound, rc, "見つかりません", "この iPhone は見つかりません。")
		return
	}
	code := secret.NewEnrollmentCode()
	expires := time.Now().Add(s.cfg.EnrollmentCodeTTL)
	if err := s.store.CreateEnrollmentCode(ctx, store.EnrollmentCode{CodeHash: secret.Hash(code), Extension: e.Number, ExpiresAt: expires}); err != nil {
		s.log.Error("web: create enrollment code", "error", err)
		s.message(w, http.StatusInternalServerError, rc, "エラー", "登録コードを発行できませんでした。")
		return
	}
	link := s.cfg.PublicURL + "/enroll?code=" + code
	png, err := qrcode.Encode(link, qrcode.Medium, 256)
	if err != nil {
		s.log.Error("web: qr", "error", err)
	}
	s.log.Info("web: enrollment code issued", "user_id", subject.ID, "extension", e.Number, "actor", rc.user.ID)
	s.audit(ctx, subject.ID, rc.user.ID, "code_issued", fmt.Sprintf("%s（内線 %s）", e.Label, e.Number))
	p := rc.page("登録コード")
	p.Data = codeData{
		Phone: phoneView{Number: e.Number, Label: e.Label}, Code: secret.FormatCode(code), PublicURL: s.cfg.PublicURL,
		QR:        template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png)), //nolint:gosec // 自分で作った PNG
		ExpiresAt: expires, Base: base,
	}
	s.render(w, http.StatusOK, "code", p)
}

func (s *Server) renamePhone(w http.ResponseWriter, r *http.Request, rc *reqCtx, subject store.User, base string) {
	ctx := r.Context()
	e, ok := s.ownedExtension(ctx, subject, r.PathValue("number"))
	if !ok {
		s.message(w, http.StatusNotFound, rc, "見つかりません", "この iPhone は見つかりません。")
		return
	}
	label := cleanLabel(r.PostFormValue("label"))
	if err := s.store.SetExtensionLabel(ctx, e.Number, label); err != nil {
		s.message(w, http.StatusInternalServerError, rc, "エラー", "名前を変更できませんでした。")
		return
	}
	s.audit(ctx, subject.ID, rc.user.ID, "phone_renamed", fmt.Sprintf("%s → %s（内線 %s）", e.Label, label, e.Number))
	http.Redirect(w, r, backTo(base, "renamed", true), http.StatusSeeOther)
}

// release は内線の割り当てを外し、iPhone の資格情報を無効にする。
func (s *Server) release(ctx context.Context, number string) (bool, error) {
	if err := s.store.ReleaseExtension(ctx, number, time.Now()); err != nil {
		return false, err
	}
	if s.cfg.Relay {
		if err := s.store.SetMobilePassword(ctx, number, s.box.Seal(relay.NewPassword())); err != nil {
			return false, err
		}
	}
	return s.applyRelay(ctx), nil
}

func (s *Server) deletePhone(w http.ResponseWriter, r *http.Request, rc *reqCtx, subject store.User, base string) {
	ctx := r.Context()
	e, ok := s.ownedExtension(ctx, subject, r.PathValue("number"))
	if !ok {
		s.message(w, http.StatusNotFound, rc, "見つかりません", "この iPhone は見つかりません。")
		return
	}
	synced, err := s.release(ctx, e.Number)
	if err != nil {
		s.log.Error("web: release extension", "extension", e.Number, "error", err)
		s.message(w, http.StatusInternalServerError, rc, "エラー", "iPhone を削除できませんでした。")
		return
	}
	s.log.Info("web: phone deleted", "user_id", subject.ID, "extension", e.Number, "actor", rc.user.ID)
	s.audit(ctx, subject.ID, rc.user.ID, "phone_deleted", fmt.Sprintf("%s（内線 %s）", e.Label, e.Number))
	http.Redirect(w, r, backTo(base, "deleted", synced), http.StatusSeeOther)
}

// --- サポート用パスコード

// newPasscode は電話や Discord で読み上げやすい数字8桁を作る（表示は 1234-5678）。
func newPasscode() string {
	var sb strings.Builder
	for i := 0; i < 8; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			panic(err)
		}
		sb.WriteByte(byte('0' + n.Int64()))
	}
	return sb.String()
}

func normalizePasscode(raw string) string {
	var sb strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r >= '０' && r <= '９':
			sb.WriteRune('0' + (r - '０'))
		}
	}
	return sb.String()
}

type passcodeData struct {
	Code      string
	ExpiresAt time.Time
	Minutes   int
}

func (s *Server) createPasscode(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	code := newPasscode()
	expires := time.Now().Add(s.cfg.Web.SupportCodeTTL)
	if err := s.store.SetSupportPasscode(ctx, store.SupportPasscode{UserID: rc.user.ID, CodeHash: secret.Hash(code), ExpiresAt: expires}); err != nil {
		s.log.Error("web: support passcode", "error", err)
		s.message(w, http.StatusInternalServerError, rc, "エラー", "サポート用パスコードを発行できませんでした。")
		return
	}
	s.audit(ctx, rc.user.ID, rc.user.ID, "passcode_issued", "")
	p := rc.page("サポート用パスコード")
	p.Data = passcodeData{Code: code[:4] + "-" + code[4:], ExpiresAt: expires, Minutes: int(s.cfg.Web.SupportSessionTTL.Minutes())}
	s.render(w, http.StatusOK, "passcode", p)
}

func (s *Server) deletePasscode(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	_ = s.store.DeleteSupportPasscode(r.Context(), rc.user.ID)
	s.audit(r.Context(), rc.user.ID, rc.user.ID, "passcode_deleted", "")
	http.Redirect(w, r, "/?m=passcode_delete", http.StatusSeeOther)
}

// supportAllowed は管理者ごとに、10分間に5回までパスコードの入力の失敗を許す。
func (s *Server) supportAllowed(adminID string, failed bool) bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	now := time.Now()
	kept := s.supportFails[adminID][:0]
	for _, t := range s.supportFails[adminID] {
		if now.Sub(t) < 10*time.Minute {
			kept = append(kept, t)
		}
	}
	if failed {
		kept = append(kept, now)
	}
	s.supportFails[adminID] = kept
	return len(kept) < 5
}

func (s *Server) startSupport(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	if !s.supportAllowed(rc.user.ID, false) {
		s.message(w, http.StatusTooManyRequests, rc, "しばらくお待ちください", "パスコードの入力に続けて失敗したため、10分ほど待ってからもう一度お試しください。")
		return
	}
	code := normalizePasscode(r.PostFormValue("passcode"))
	userID, err := s.store.ConsumeSupportPasscode(ctx, secret.Hash(code), time.Now())
	if err != nil || len(code) != 8 {
		s.supportAllowed(rc.user.ID, true)
		s.log.Warn("web: support passcode rejected", "admin", rc.user.ID)
		s.message(w, http.StatusForbidden, rc, "パスコードが違います", "パスコードが正しくないか、有効期限が切れています。利用者に新しいパスコードを発行してもらってください。")
		return
	}
	until := time.Now().Add(s.cfg.Web.SupportSessionTTL)
	if err := s.store.SetWebSessionSupport(ctx, rc.sess.Hash, userID, &until); err != nil {
		s.message(w, http.StatusInternalServerError, rc, "エラー", "サポートを開始できませんでした。")
		return
	}
	s.log.Info("web: support started", "admin", rc.user.ID, "user_id", userID, "until", until)
	s.audit(ctx, userID, rc.user.ID, "support_started", "期限 "+until.Local().Format("15:04"))
	http.Redirect(w, r, "/support", http.StatusSeeOther)
}

func (s *Server) endSupport(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	if rc.sess.SupportUserID != "" {
		s.audit(ctx, rc.sess.SupportUserID, rc.user.ID, "support_ended", "")
	}
	_ = s.store.SetWebSessionSupport(ctx, rc.sess.Hash, "", nil)
	http.Redirect(w, r, "/admin?m=support_end", http.StatusSeeOther)
}

// --- 管理者の画面

type adminUserView struct {
	User    store.User
	Phones  int
	IsAdmin bool
}

type adminExtensionView struct {
	Ext       store.Extension
	OwnerName string
	Enrolled  bool
	InRelay   bool
}

type adminData struct {
	Users      []adminUserView
	Extensions []adminExtensionView
	Relay      bool
	FreeCount  int
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		s.message(w, http.StatusInternalServerError, rc, "エラー", "画面を表示できませんでした。")
		return
	}
	exts, err := s.store.ListExtensions(ctx)
	if err != nil {
		s.message(w, http.StatusInternalServerError, rc, "エラー", "画面を表示できませんでした。")
		return
	}
	names := map[string]string{}
	counts := map[string]int{}
	for _, u := range users {
		names[u.ID] = u.DisplayName
	}
	data := adminData{Relay: s.cfg.Relay}
	for _, e := range exts {
		v := adminExtensionView{Ext: e, OwnerName: names[e.OwnerUserID], InRelay: !e.Pool || e.OwnerUserID != ""}
		if e.OwnerUserID != "" {
			counts[e.OwnerUserID]++
		} else if e.Pool {
			data.FreeCount++
		}
		if _, err := s.store.ActiveDeviceForExtension(ctx, e.Number); err == nil {
			v.Enrolled = true
		}
		data.Extensions = append(data.Extensions, v)
	}
	for _, u := range users {
		data.Users = append(data.Users, adminUserView{User: u, Phones: counts[u.ID], IsAdmin: s.isAdminID(u.DiscordID)})
	}
	p := rc.page("管理")
	p.Notice = noticeFrom(r)
	p.Data = data
	s.render(w, http.StatusOK, "admin", p)
}

// saveExtension は MikoPBX で作った内線を登録・更新する（SIP パスワードは暗号化して保存する）。
func (s *Server) saveExtension(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	number := strings.TrimSpace(r.PostFormValue("number"))
	sipUser := strings.TrimSpace(r.PostFormValue("sip_user"))
	authUser := strings.TrimSpace(r.PostFormValue("auth_user"))
	password := r.PostFormValue("password")
	if sipUser == "" {
		sipUser = number
	}
	if authUser == "" {
		authUser = sipUser
	}
	if !validName(number) || !validName(sipUser) || !validName(authUser) {
		s.message(w, http.StatusBadRequest, rc, "保存できません", "内線番号・SIP ユーザー名には英数字と . _ + - だけを使えます。")
		return
	}
	if password == "" {
		s.message(w, http.StatusBadRequest, rc, "保存できません", "SIP パスワード（MikoPBX の内線設定の値）を入力してください。")
		return
	}
	existing, err := s.store.GetExtension(ctx, number)
	ring := 30
	if err == nil {
		ring = existing.RingTimeoutSeconds
	}
	if err := s.store.UpsertExtension(ctx, store.Extension{
		Number: number, DisplayName: strings.TrimSpace(r.PostFormValue("display_name")), SIPUsername: sipUser, SIPAuthUsername: authUser,
		SIPPasswordEnc: s.box.Seal(password), Endpoint: sipUser, RingTimeoutSeconds: ring,
	}); err != nil {
		s.log.Error("web: save extension", "error", err)
		s.message(w, http.StatusInternalServerError, rc, "エラー", "内線を保存できませんでした。")
		return
	}
	if err := s.store.SetExtensionPool(ctx, number, r.PostFormValue("pool") == "on"); err != nil {
		s.log.Error("web: set pool", "error", err)
	}
	s.log.Info("web: extension saved", "extension", number, "admin", rc.user.ID)
	ok := s.applyRelay(ctx)
	http.Redirect(w, r, backTo("/admin", "saved", ok), http.StatusSeeOther)
}

func validName(v string) bool {
	if v == "" || len(v) > 40 {
		return false
	}
	for _, r := range v {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune("._+-", r)) {
			return false
		}
	}
	return true
}

func (s *Server) setPool(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	number := r.PathValue("number")
	if err := s.store.SetExtensionPool(ctx, number, r.PostFormValue("pool") == "on"); err != nil {
		s.message(w, http.StatusNotFound, rc, "見つかりません", "内線が見つかりません。")
		return
	}
	ok := s.applyRelay(ctx)
	http.Redirect(w, r, backTo("/admin", "saved", ok), http.StatusSeeOther)
}

// assignExtension は未割り当ての内線を利用者に割り当てる（すでに使っている内線を利用者の管理に移すときなど）。
func (s *Server) assignExtension(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	number := r.PathValue("number")
	user, err := s.store.GetUser(ctx, r.PostFormValue("user_id"))
	if err != nil {
		s.message(w, http.StatusBadRequest, rc, "割り当てできません", "利用者を選んでください。")
		return
	}
	label := cleanLabel(r.PostFormValue("label"))
	if err := s.store.AssignExtension(ctx, number, user.ID, label, time.Now()); err != nil {
		s.message(w, http.StatusConflict, rc, "割り当てできません", "内線が見つからないか、すでにほかの利用者に割り当てられています。")
		return
	}
	s.log.Info("web: extension assigned", "extension", number, "user_id", user.ID, "admin", rc.user.ID)
	s.audit(ctx, user.ID, rc.user.ID, "phone_assigned", fmt.Sprintf("%s（内線 %s）", label, number))
	ok := s.applyRelay(ctx)
	http.Redirect(w, r, backTo("/admin", "saved", ok), http.StatusSeeOther)
}

func (s *Server) releaseExtension(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	e, err := s.store.GetExtension(ctx, r.PathValue("number"))
	if err != nil || e.OwnerUserID == "" {
		s.message(w, http.StatusNotFound, rc, "見つかりません", "割り当て済みの内線が見つかりません。")
		return
	}
	synced, err := s.release(ctx, e.Number)
	if err != nil {
		s.log.Error("web: release extension", "extension", e.Number, "error", err)
		s.message(w, http.StatusInternalServerError, rc, "エラー", "割り当てを解除できませんでした。")
		return
	}
	s.log.Info("web: extension released", "extension", e.Number, "user_id", e.OwnerUserID, "admin", rc.user.ID)
	s.audit(ctx, e.OwnerUserID, rc.user.ID, "phone_released", fmt.Sprintf("%s（内線 %s）", e.Label, e.Number))
	http.Redirect(w, r, backTo("/admin", "saved", synced), http.StatusSeeOther)
}

func (s *Server) setUserDisabled(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	id := r.PathValue("id")
	disabled := r.PostFormValue("disabled") == "1"
	if id == rc.user.ID && disabled {
		s.message(w, http.StatusBadRequest, rc, "停止できません", "自分自身は停止できません。")
		return
	}
	if err := s.store.SetUserDisabled(ctx, id, disabled); err != nil {
		s.message(w, http.StatusNotFound, rc, "見つかりません", "利用者が見つかりません。")
		return
	}
	if disabled {
		// ログイン中の画面もすぐ使えなくする（iPhone の着信・発信は止めない）
		_ = s.store.DeleteUserWebSessions(ctx, id)
	}
	s.log.Info("web: user disabled changed", "user_id", id, "disabled", disabled, "admin", rc.user.ID)
	http.Redirect(w, r, "/admin?m=saved", http.StatusSeeOther)
}
