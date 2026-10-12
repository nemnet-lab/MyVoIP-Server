// Package relay は中継モードの Asterisk 設定を生成する。
//
// 中継モードでは、VoIP Push サーバー内の Asterisk が MikoPBX に内線（例 9002）として常時登録する。
// MikoPBX から見ると、その内線は常に online の通常の端末になり、画面で設定した着信ルートがそのまま使える。
// iPhone は MikoPBX ではなく中継用 Asterisk に登録し、着信は Stasis（呼制御）を通して Push で起こしてからつなぐ。
// iPhone からの発信は、中継用 Asterisk が MikoPBX へその内線からの発信として送る。
//
// 利用者ごとに自分の PBX（MikoPBX・Asterisk）を持つ構成では、内線ごとに持ち主の PBX へ登録する。
package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

// mobileRTPTimeout は、iPhone からの音声が途絶えた通話を切るまでの秒数。
// 回線切り替えでの一時的な途絶（アプリの再接続は最長10秒）より十分長くする。
const mobileRTPTimeout = 60

// FileName は pjsip.conf から #tryinclude される生成ファイル。
const FileName = "pjsip_myvoip.conf"

// MOHFileName は musiconhold.conf から #tryinclude される生成ファイル（利用者の保留音のクラス）。
const MOHFileName = "musiconhold_myvoip.conf"

// DefaultAsteriskDir は中継用 Asterisk から見た共有ディレクトリ（docker-compose.yml のボリューム）。
const DefaultAsteriskDir = "/etc/asterisk/myvoip"

// MOHClass は保留音の Asterisk のクラス名。
func MOHClass(id string) string { return "myvoip-moh-" + id }

// safeMOHID はファイル名・設定に使ってよい保留音の ID（サーバーが作る UUID）。
var safeMOHID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Options struct {
	// UpstreamHost / Port は既定の PBX（持ち主のいない内線と、PBX を設定していない利用者の内線の登録先）
	UpstreamHost string
	UpstreamPort int
	// UserPBX は利用者ごとの PBX（利用者 ID → 接続先）
	UserPBX map[string]Upstream
	// AllowedClients は iPhone 用エンドポイントに SIP で接続できるアドレス帯（空なら制限しない）
	AllowedClients []netip.Prefix
	// SRTP は optional / mandatory / disabled（mandatory のときだけ iPhone 側を sdes にする）
	SRTP string
	// MOH は書き出した保留音の ID（ここにある保留音だけを内線に割り当てる）
	MOH map[string]bool
	// AsteriskDir は中継用 Asterisk から見た共有ディレクトリ（空なら DefaultAsteriskDir）
	AsteriskDir string
}

// Upstream は中継用 Asterisk が登録する PBX の SIP の接続先。
type Upstream struct {
	Host string
	Port int
}

// upstreamFor は内線の登録先を返す。利用者の PBX があればそちら、なければ既定の PBX。
func (o Options) upstreamFor(e store.Extension) (Upstream, bool) {
	if e.OwnerUserID != "" {
		if u, ok := o.UserPBX[e.OwnerUserID]; ok && u.Host != "" {
			return u, true
		}
	}
	if o.UpstreamHost != "" {
		return Upstream{Host: o.UpstreamHost, Port: o.UpstreamPort}, true
	}
	return Upstream{}, false
}

var (
	safeName = regexp.MustCompile(`^[0-9A-Za-z_.+-]+$`)
	safeHost = regexp.MustCompile(`^[0-9A-Za-z.:-]+$`)
)

// validUpstream は設定ファイルにそのまま書いてよい接続先かを確かめる。
func validUpstream(u Upstream) bool {
	return safeHost.MatchString(u.Host) && !strings.HasPrefix(u.Host, "-") && u.Port > 0 && u.Port < 65536
}

// UpstreamEndpoint は MikoPBX 側のエンドポイント名（iPhone からの発信の宛先）。
func UpstreamEndpoint(number string) string { return "myvoip-up-" + number }

// EnsureMobilePasswords は iPhone 用パスワードのない内線に新しいパスワードを発行する。
func EnsureMobilePasswords(ctx context.Context, st store.Store, box *secret.Box) error {
	exts, err := st.ListExtensions(ctx)
	if err != nil {
		return err
	}
	for _, e := range exts {
		if len(e.MobilePasswordEnc) > 0 {
			continue
		}
		if err := st.SetMobilePassword(ctx, e.Number, box.Seal(NewPassword())); err != nil {
			return err
		}
	}
	return nil
}

// NewPassword は iPhone 用の SIP パスワードを作る。
func NewPassword() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Render は内線一覧から pjsip の設定を作る。
// 登録先の PBX がない・設定に使えない文字があるなどの内線は書かずに、理由を skipped に入れる（ほかの内線は止めない）。
// コーデックは MikoPBX の既定（PCMA 優先）に合わせて alaw,ulaw の順にし、両側で同じコーデックになるようにする。
func Render(exts []store.Extension, box *secret.Box, opts Options) (content string, skipped []string) {
	sort.Slice(exts, func(i, j int) bool { return exts[i].Number < exts[j].Number })
	var b strings.Builder
	b.WriteString("; MyVoIP-Server が生成する設定です。手で編集しないでください（extension set や relay sync で上書きされます）。\n")
	for _, e := range exts {
		if err := renderExtension(&b, e, box, opts); err != nil {
			skipped = append(skipped, fmt.Sprintf("extension %s: %v", e.Number, err))
		}
	}
	return b.String(), skipped
}

func renderExtension(out *strings.Builder, e store.Extension, box *secret.Box, opts Options) error {
	authUser := e.SIPAuthUsername
	if authUser == "" {
		authUser = e.SIPUsername
	}
	if !safeName.MatchString(e.Number) || !safeName.MatchString(e.SIPUsername) || !safeName.MatchString(authUser) {
		return errors.New("has characters that cannot be used in the relay configuration")
	}
	upstream, ok := opts.upstreamFor(e)
	if !ok {
		return errors.New("no PBX to register to (set the user's PBX or UPSTREAM_SIP_HOST)")
	}
	if !validUpstream(upstream) {
		return fmt.Errorf("invalid PBX address %q", upstream.Host)
	}
	upstreamPassword, err := box.Open(e.SIPPasswordEnc)
	if err != nil {
		return err
	}
	if len(e.MobilePasswordEnc) == 0 {
		return errors.New("has no mobile password")
	}
	mobilePassword, err := box.Open(e.MobilePasswordEnc)
	if err != nil {
		return err
	}
	if !safeValue(upstreamPassword) || !safeValue(mobilePassword) {
		return errors.New("password has control characters")
	}
	host := upstream.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6
	}
	up := UpstreamEndpoint(e.Number)
	server := fmt.Sprintf("sip:%s:%d", host, upstream.Port)

	// 一時的に組み立ててから書く（途中で失敗した内線を半端に書かない）
	var b strings.Builder
	fmt.Fprintf(&b, "\n; ===== 内線 %s：PBX %s への登録（PBX から見た内線 %s） =====\n", e.Number, upstream.Host, e.SIPUsername)
	fmt.Fprintf(&b, "[%s-auth]\ntype=auth\nauth_type=userpass\nusername=%s\npassword=%s\n\n", up, authUser, escape(upstreamPassword))
	// PBX への生存確認（OPTIONS）で、PBX に届くかを把握し、経路（Tailscale の通信の状態）を保つ
	fmt.Fprintf(&b, "[%s]\ntype=aor\ncontact=%s\nqualify_frequency=30\nqualify_timeout=5\n\n", up, server)
	// PBX からの INVITE は、登録の line パラメーターだけで見分ける（identify_by=ip で、From のユーザー名では一致させない。
	// 認証なしのエンドポイントなので、ほかの利用者がユーザー名をまねて着信させられないようにする）
	fmt.Fprintf(&b, "[%s]\ntype=endpoint\ntransport=transport-udp\ncontext=from-pbx\ndisallow=all\nallow=alaw,ulaw\n"+
		"outbound_auth=%s-auth\naors=%s\nfrom_user=%s\nfrom_domain=%s\ndirect_media=no\nrtp_symmetric=yes\n"+
		"force_rport=yes\nrewrite_contact=yes\ndtmf_mode=rfc4733\nidentify_by=ip\nset_var=MYVOIP_EXT=%s\n\n",
		up, up, up, e.SIPUsername, upstream.Host, e.Number)
	fmt.Fprintf(&b, "[%s]\ntype=registration\ntransport=transport-udp\noutbound_auth=%s-auth\nserver_uri=%s\n"+
		"client_uri=sip:%s@%s\ncontact_user=%s\nexpiration=300\nretry_interval=30\nforbidden_retry_interval=120\n"+
		"fatal_retry_interval=120\nmax_retries=1000000\nauth_rejection_permanent=no\nline=yes\nendpoint=%s\n",
		up, up, server, e.SIPUsername, host, e.SIPUsername, up)

	fmt.Fprintf(&b, "\n; ===== 内線 %s：iPhone の登録先 =====\n", e.Number)
	fmt.Fprintf(&b, "[%s]\ntype=auth\nauth_type=userpass\nusername=%s\npassword=%s\n\n", e.Number, e.Number, escape(mobilePassword))
	// アプリは接続のたびに新しい Contact で登録するので複数を許す。生存確認（OPTIONS）は不要
	fmt.Fprintf(&b, "[%s]\ntype=aor\nmax_contacts=5\nremove_existing=no\nqualify_frequency=0\nminimum_expiration=60\ndefault_expiration=300\n\n", e.Number)
	encryption := "no"
	if opts.SRTP == "mandatory" {
		encryption = "sdes"
	}
	// アプリが通話中に強制終了されるなどして BYE が届かなかったときに、PBX 側の通話を残さないよう、
	// 音声が届かなくなった通話を切る（保留中は対象外。ミュート中もアプリは無音を送り続ける）
	fmt.Fprintf(&b, "[%s]\ntype=endpoint\ntransport=transport-udp\ncontext=from-mobile\ndisallow=all\nallow=alaw,ulaw\n"+
		"auth=%s\naors=%s\ndirect_media=no\nrtp_symmetric=yes\nforce_rport=yes\nrewrite_contact=yes\n"+
		"dtmf_mode=rfc4733\nmedia_encryption=%s\nrtp_timeout=%d\nset_var=MYVOIP_EXT=%s\n",
		e.Number, e.Number, e.Number, encryption, mobileRTPTimeout, e.Number)
	// iPhone が保留したとき、相手（PBX 側）に流す保留音。iPhone 用のエンドポイントにだけ付ける
	//（保留した側のエンドポイントの moh_suggest が、相手のチャネルの保留音のクラスになる）
	if e.MOHFileID != "" && opts.MOH[e.MOHFileID] {
		fmt.Fprintf(&b, "moh_suggest=%s\n", MOHClass(e.MOHFileID))
	}
	if len(opts.AllowedClients) > 0 {
		b.WriteString("deny=0.0.0.0/0.0.0.0\ndeny=::/0\n")
		for _, p := range opts.AllowedClients {
			fmt.Fprintf(&b, "permit=%s\n", p.String())
		}
	}
	out.WriteString(b.String())
	return nil
}

// escape は pjsip.conf の値として使えるようにする（; はコメント扱いになるため）。
func escape(s string) string {
	return strings.ReplaceAll(s, ";", `\;`)
}

// safeValue は改行などの制御文字を含まない（設定ファイルの行を壊さない）ことを確かめる。
func safeValue(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Write は設定ファイルを置き換える（途中の状態を Asterisk に読ませない）。
func Write(dir, content string) error { return writeFile(dir, FileName, []byte(content)) }

func writeFile(dir, name string, content []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".myvoip-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Asterisk（別ユーザー）が読めるようにする
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// Reloader は Asterisk のモジュールを再読み込みする（ARI）。
type Reloader interface {
	ReloadModule(ctx context.Context, name string) error
}

// Result は Sync の結果。
type Result struct {
	// Skipped は書かなかった内線と理由
	Skipped []string
}

// Sync は内線一覧から設定を書き出し、Asterisk に読み込ませる。
func Sync(ctx context.Context, st store.Store, box *secret.Box, opts Options, dir string, reloader Reloader) (Result, error) {
	if err := EnsureMobilePasswords(ctx, st, box); err != nil {
		return Result{}, err
	}
	exts, err := st.ListExtensions(ctx)
	if err != nil {
		return Result{}, err
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return Result{}, err
	}
	opts.UserPBX = map[string]Upstream{}
	for _, u := range users {
		if u.PBXHost != "" {
			opts.UserPBX[u.ID] = Upstream{Host: u.PBXHost, Port: u.PBXPort}
		}
	}
	result := Result{}
	// 保留音は、音声を先に書いてからクラスを定義する（クラスのディレクトリが空のまま読ませない）
	opts.MOH, result.Skipped, err = syncMOH(ctx, st, opts, dir)
	if err != nil {
		return result, err
	}
	content, skipped := Render(exts, box, opts)
	result.Skipped = append(result.Skipped, skipped...)
	if err := Write(dir, content); err != nil {
		return result, err
	}
	if reloader != nil {
		for _, module := range []string{"res_musiconhold.so", "res_pjsip.so", "res_pjsip_outbound_registration.so"} {
			if err := reloader.ReloadModule(ctx, module); err != nil {
				return result, fmt.Errorf("reload %s: %w", module, err)
			}
		}
	}
	// 消した保留音の音声は、新しい設定を読ませてから消す
	removeStaleMOH(dir, opts.MOH)
	return result, nil
}

// syncMOH は保留音の音声を共有ディレクトリに書き出し（まだないものだけ）、クラスの設定を書く。
// 音声は利用者ごとではなく保留音ごとのディレクトリに置き、クラス名・パスには利用者が付けた名前を使わない。
func syncMOH(ctx context.Context, st store.Store, opts Options, dir string) (map[string]bool, []string, error) {
	files, err := st.ListAllMOH(ctx)
	if err != nil {
		return nil, nil, err
	}
	asteriskDir := opts.AsteriskDir
	if asteriskDir == "" {
		asteriskDir = DefaultAsteriskDir
	}
	available := map[string]bool{}
	var skipped []string
	var conf strings.Builder
	conf.WriteString("; MyVoIP-Server が生成する設定です。手で編集しないでください（利用者の保留音）。\n")
	for _, f := range files {
		if !safeMOHID.MatchString(f.ID) {
			skipped = append(skipped, fmt.Sprintf("moh %q: invalid id", f.ID))
			continue
		}
		mohDir := filepath.Join(dir, "moh", f.ID)
		if _, err := os.Stat(filepath.Join(mohDir, "hold.wav")); err != nil {
			data, err := st.GetMOHData(ctx, f.ID)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("moh %s: %v", f.ID, err))
				continue
			}
			if err := os.MkdirAll(mohDir, 0o755); err != nil {
				return nil, nil, err
			}
			if err := writeFile(mohDir, "hold.wav", data); err != nil {
				return nil, nil, err
			}
		}
		available[f.ID] = true
		fmt.Fprintf(&conf, "\n[%s]\nmode=files\ndirectory=%s/moh/%s\nsort=alpha\n", MOHClass(f.ID), asteriskDir, f.ID)
	}
	if err := writeFile(dir, MOHFileName, []byte(conf.String())); err != nil {
		return nil, nil, err
	}
	return available, skipped, nil
}

func removeStaleMOH(dir string, keep map[string]bool) {
	entries, err := os.ReadDir(filepath.Join(dir, "moh"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && safeMOHID.MatchString(e.Name()) && !keep[e.Name()] {
			_ = os.RemoveAll(filepath.Join(dir, "moh", e.Name()))
		}
	}
}
