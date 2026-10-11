// Package relay は中継モードの Asterisk 設定を生成する。
//
// 中継モードでは、VoIP Push サーバー内の Asterisk が MikoPBX に内線（例 9002）として常時登録する。
// MikoPBX から見ると、その内線は常に online の通常の端末になり、画面で設定した着信ルートがそのまま使える。
// iPhone は MikoPBX ではなく中継用 Asterisk に登録し、着信は Stasis（呼制御）を通して Push で起こしてからつなぐ。
// iPhone からの発信は、中継用 Asterisk が MikoPBX へその内線からの発信として送る。
package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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

// FileName は pjsip.conf から #tryinclude される生成ファイル。
const FileName = "pjsip_myvoip.conf"

type Options struct {
	UpstreamHost string
	UpstreamPort int
	// AllowedClients は iPhone 用エンドポイントに SIP で接続できるアドレス帯（空なら制限しない）
	AllowedClients []netip.Prefix
	// SRTP は optional / mandatory / disabled（mandatory のときだけ iPhone 側を sdes にする）
	SRTP string
}

var safeName = regexp.MustCompile(`^[0-9A-Za-z_.+-]+$`)

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
// コーデックは MikoPBX の既定（PCMA 優先）に合わせて alaw,ulaw の順にし、両側で同じコーデックになるようにする。
func Render(exts []store.Extension, box *secret.Box, opts Options) (string, error) {
	sort.Slice(exts, func(i, j int) bool { return exts[i].Number < exts[j].Number })
	var b strings.Builder
	b.WriteString("; MyVoIP-Server が生成する設定です。手で編集しないでください（extension set や relay sync で上書きされます）。\n")
	for _, e := range exts {
		if !safeName.MatchString(e.Number) || !safeName.MatchString(e.SIPUsername) {
			return "", fmt.Errorf("extension %q has characters that cannot be used in the relay configuration", e.Number)
		}
		upstreamPassword, err := box.Open(e.SIPPasswordEnc)
		if err != nil {
			return "", fmt.Errorf("extension %s: %w", e.Number, err)
		}
		if len(e.MobilePasswordEnc) == 0 {
			return "", fmt.Errorf("extension %s has no mobile password", e.Number)
		}
		mobilePassword, err := box.Open(e.MobilePasswordEnc)
		if err != nil {
			return "", fmt.Errorf("extension %s: %w", e.Number, err)
		}
		authUser := e.SIPAuthUsername
		if authUser == "" {
			authUser = e.SIPUsername
		}
		up := UpstreamEndpoint(e.Number)
		server := fmt.Sprintf("sip:%s:%d", opts.UpstreamHost, opts.UpstreamPort)

		fmt.Fprintf(&b, "\n; ===== 内線 %s：MikoPBX への登録（MikoPBX から見た内線 %s） =====\n", e.Number, e.SIPUsername)
		fmt.Fprintf(&b, "[%s-auth]\ntype=auth\nauth_type=userpass\nusername=%s\npassword=%s\n\n", up, authUser, escape(upstreamPassword))
		fmt.Fprintf(&b, "[%s]\ntype=aor\ncontact=%s\n\n", up, server)
		fmt.Fprintf(&b, "[%s]\ntype=endpoint\ntransport=transport-udp\ncontext=from-pbx\ndisallow=all\nallow=alaw,ulaw\n"+
			"outbound_auth=%s-auth\naors=%s\nfrom_user=%s\nfrom_domain=%s\ndirect_media=no\nrtp_symmetric=yes\n"+
			"force_rport=yes\nrewrite_contact=yes\ndtmf_mode=rfc4733\nset_var=MYVOIP_EXT=%s\n\n",
			up, up, up, e.SIPUsername, opts.UpstreamHost, e.Number)
		fmt.Fprintf(&b, "[%s]\ntype=registration\ntransport=transport-udp\noutbound_auth=%s-auth\nserver_uri=%s\n"+
			"client_uri=sip:%s@%s\ncontact_user=%s\nexpiration=300\nretry_interval=30\nforbidden_retry_interval=120\n"+
			"fatal_retry_interval=120\nmax_retries=1000000\nauth_rejection_permanent=no\nline=yes\nendpoint=%s\n",
			up, up, server, e.SIPUsername, opts.UpstreamHost, e.SIPUsername, up)

		fmt.Fprintf(&b, "\n; ===== 内線 %s：iPhone の登録先 =====\n", e.Number)
		fmt.Fprintf(&b, "[%s]\ntype=auth\nauth_type=userpass\nusername=%s\npassword=%s\n\n", e.Number, e.Number, escape(mobilePassword))
		// アプリは接続のたびに新しい Contact で登録するので複数を許す。生存確認（OPTIONS）は不要
		fmt.Fprintf(&b, "[%s]\ntype=aor\nmax_contacts=5\nremove_existing=no\nqualify_frequency=0\nminimum_expiration=60\ndefault_expiration=300\n\n", e.Number)
		encryption := "no"
		if opts.SRTP == "mandatory" {
			encryption = "sdes"
		}
		fmt.Fprintf(&b, "[%s]\ntype=endpoint\ntransport=transport-udp\ncontext=from-mobile\ndisallow=all\nallow=alaw,ulaw\n"+
			"auth=%s\naors=%s\ndirect_media=no\nrtp_symmetric=yes\nforce_rport=yes\nrewrite_contact=yes\n"+
			"dtmf_mode=rfc4733\nmedia_encryption=%s\nset_var=MYVOIP_EXT=%s\n",
			e.Number, e.Number, e.Number, encryption, e.Number)
		if len(opts.AllowedClients) > 0 {
			b.WriteString("deny=0.0.0.0/0.0.0.0\ndeny=::/0\n")
			for _, p := range opts.AllowedClients {
				fmt.Fprintf(&b, "permit=%s\n", p.String())
			}
		}
	}
	return b.String(), nil
}

// escape は pjsip.conf の値として使えるようにする（; はコメント扱いになるため）。
func escape(s string) string {
	return strings.ReplaceAll(s, ";", `\;`)
}

// InUse は MikoPBX に登録する内線を返す。
// 利用者に割り当てる予備の内線（pool）は、割り当てるまで登録しない（MikoPBX から見て offline のまま）。
func InUse(exts []store.Extension) []store.Extension {
	var list []store.Extension
	for _, e := range exts {
		if !e.Pool || e.OwnerUserID != "" {
			list = append(list, e)
		}
	}
	return list
}

// Write は設定ファイルを置き換える（途中の状態を Asterisk に読ませない）。
func Write(dir, content string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pjsip_myvoip-*.conf")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
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
	return os.Rename(tmp.Name(), filepath.Join(dir, FileName))
}

// Reloader は Asterisk のモジュールを再読み込みする（ARI）。
type Reloader interface {
	ReloadModule(ctx context.Context, name string) error
}

// Sync は内線一覧から設定を書き出し、Asterisk に読み込ませる。
func Sync(ctx context.Context, st store.Store, box *secret.Box, opts Options, dir string, reloader Reloader) error {
	if err := EnsureMobilePasswords(ctx, st, box); err != nil {
		return err
	}
	exts, err := st.ListExtensions(ctx)
	if err != nil {
		return err
	}
	content, err := Render(InUse(exts), box, opts)
	if err != nil {
		return err
	}
	if err := Write(dir, content); err != nil {
		return err
	}
	if reloader == nil {
		return nil
	}
	if err := reloader.ReloadModule(ctx, "res_pjsip.so"); err != nil {
		return fmt.Errorf("reload res_pjsip: %w", err)
	}
	if err := reloader.ReloadModule(ctx, "res_pjsip_outbound_registration.so"); err != nil {
		return fmt.Errorf("reload res_pjsip_outbound_registration: %w", err)
	}
	return nil
}
