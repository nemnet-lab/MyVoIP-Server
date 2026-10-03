// myvoip-server は着信制御・Push 送信サービスと、その管理コマンド。
//
//	myvoip-server serve                       サーバーを起動する
//	myvoip-server extension set ...           MikoPBX の既存内線をアプリ用に登録する
//	myvoip-server extension list
//	myvoip-server enroll-code --extension 201 登録コード（10分・1回限り）と QR を発行する
//	myvoip-server devices                     端末一覧
//	myvoip-server revoke-device --device ID   端末の資格情報・Push 紐付けを失効する
//	myvoip-server check-ari                   ARI への接続と内線の Contact を確認する
//	myvoip-server gen-secret                  MYVOIP_SECRET_KEY 用の鍵を作る
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // 最小イメージでも TZ=Asia/Tokyo を使えるようにする

	"github.com/nemnet-lab/MyVoIP-Server/internal/api"
	"github.com/nemnet-lab/MyVoIP-Server/internal/apns"
	"github.com/nemnet-lab/MyVoIP-Server/internal/ari"
	"github.com/nemnet-lab/MyVoIP-Server/internal/calls"
	"github.com/nemnet-lab/MyVoIP-Server/internal/config"
	"github.com/nemnet-lab/MyVoIP-Server/internal/qr"
	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(log)
	case "extension":
		err = extensionCmd(os.Args[2:])
	case "enroll-code":
		err = enrollCode(os.Args[2:])
	case "devices":
		err = devices()
	case "revoke-device":
		err = revokeDevice(os.Args[2:])
	case "check-ari":
		err = checkARI(os.Args[2:])
	case "healthcheck":
		err = healthcheck()
	case "gen-secret":
		fmt.Println(secret.NewKey())
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `使い方:
  myvoip-server serve
  myvoip-server extension set --number 201 [--display-name 名前] [--sip-user 201] [--auth-user 201] [--endpoint 201] [--ring-timeout 30]
      （SIP パスワードは標準入力または環境変数 SIP_PASSWORD で渡す）
  myvoip-server extension list
  myvoip-server enroll-code --extension 201
  myvoip-server devices
  myvoip-server revoke-device --device d-xxxx
  myvoip-server check-ari [--extension 201]
  myvoip-server gen-secret`)
}

func openStore(ctx context.Context, cfg config.Config) (*store.Postgres, error) {
	db, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load(true)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	box, err := secret.NewBox(cfg.SecretKey)
	if err != nil {
		return err
	}
	key, err := apns.LoadKey(cfg.APNSKeyFile)
	if err != nil {
		return err
	}
	pusher := apns.New(key, cfg.APNSKeyID, cfg.APNSTeamID, cfg.APNSBundleID)
	ariClient := ari.New(cfg.ARIURL, cfg.ARIUser, cfg.ARIPassword, cfg.ARIApp, log)
	controller := calls.New(db, ariClient, pusher, log, calls.Options{RingTimeout: cfg.RingTimeout, TestSound: cfg.TestCallSound})

	// 再起動前の呼を整理してから待ち受ける。ARI に届かなくても DB 上は終了させる（R-05）
	controller.Reconcile(ctx)
	go func() {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := ariClient.Ping(pingCtx); err != nil {
			log.Warn("ARI not reachable; will keep retrying", "error", err)
		}
	}()
	go ariClient.Run(ctx, controller.HandleEvent)
	go retentionLoop(ctx, db, cfg.HistoryRetention, log)

	apiServer := api.New(db, controller, box, api.Config{
		SIPDomain:       cfg.SIPDomain,
		SIPProxyHost:    cfg.SIPProxyHost,
		SIPProxyPort:    cfg.SIPProxyPort,
		SIPTransport:    cfg.SIPTransport,
		SIPSRTP:         cfg.SIPSRTP,
		BundleID:        cfg.APNSBundleID,
		AccessTokenTTL:  cfg.AccessTokenTTL,
		RefreshTokenTTL: cfg.RefreshTokenTTL,
		RingTimeout:     cfg.RingTimeout,
	}, log)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Info("myvoip-server listening", "addr", cfg.Listen, "public_url", cfg.PublicURL, "ari_app", cfg.ARIApp,
		"sip_transport", cfg.SIPTransport, "sip_port", cfg.SIPProxyPort, "srtp", cfg.SIPSRTP)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck はコンテナのヘルスチェック用（最小イメージにはシェルがないため）。
func healthcheck() error {
	addr := os.Getenv("MYVOIP_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// retentionLoop は1日1回、180日を過ぎた履歴などを削除する（02 §6）。
func retentionLoop(ctx context.Context, db store.Store, retention time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		n, err := db.PurgeHistoryBefore(ctx, time.Now().Add(-retention))
		if err != nil {
			log.Error("retention purge failed", "error", err)
		} else if n > 0 {
			log.Info("old history purged", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// --- 管理コマンド

func adminContext() (context.Context, config.Config, *store.Postgres, error) {
	cfg, err := config.Load(false)
	if err != nil {
		return nil, cfg, nil, err
	}
	ctx := context.Background()
	db, err := openStore(ctx, cfg)
	return ctx, cfg, db, err
}

func extensionCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("extension set|list")
	}
	switch args[0] {
	case "list":
		ctx, _, db, err := adminContext()
		if err != nil {
			return err
		}
		defer db.Close()
		list, err := db.ListExtensions(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-8s %-20s %-10s %-10s %s\n", "内線", "表示名", "SIPユーザー", "エンドポイント", "呼出秒")
		for _, e := range list {
			fmt.Printf("%-8s %-20s %-10s %-10s %d\n", e.Number, e.DisplayName, e.SIPUsername, e.Endpoint, e.RingTimeoutSeconds)
		}
		return nil
	case "set":
	default:
		return errors.New("extension set|list")
	}
	fs := flag.NewFlagSet("extension set", flag.ExitOnError)
	number := fs.String("number", "", "内線番号（MikoPBX の既存内線）")
	display := fs.String("display-name", "", "表示名")
	sipUser := fs.String("sip-user", "", "SIP ユーザー名（省略時は内線番号）")
	authUser := fs.String("auth-user", "", "SIP 認証ユーザー名（省略時は SIP ユーザー名）")
	endpoint := fs.String("endpoint", "", "端末レッグを発呼する PJSIP エンドポイント名（省略時は SIP ユーザー名）")
	ring := fs.Int("ring-timeout", 30, "呼出秒数（15〜60）")
	_ = fs.Parse(args[1:])
	if *number == "" {
		return errors.New("--number is required")
	}
	if *ring < 15 || *ring > 60 {
		return errors.New("--ring-timeout must be 15..60")
	}
	if *sipUser == "" {
		*sipUser = *number
	}
	if *authUser == "" {
		*authUser = *sipUser
	}
	if *endpoint == "" {
		*endpoint = *sipUser
	}
	// SIP パスワードはコマンド引数に書かない（シェル履歴に残さない）
	password := os.Getenv("SIP_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "SIP パスワード（MikoPBX の内線設定の値）: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("SIP パスワードを入力してください")
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if password == "" {
		return errors.New("SIP パスワードが空です")
	}
	ctx, cfg, db, err := adminContext()
	if err != nil {
		return err
	}
	defer db.Close()
	box, err := secret.NewBox(cfg.SecretKey)
	if err != nil {
		return err
	}
	err = db.UpsertExtension(ctx, store.Extension{
		Number: *number, DisplayName: *display, SIPUsername: *sipUser, SIPAuthUsername: *authUser,
		SIPPasswordEnc: box.Seal(password), Endpoint: *endpoint, RingTimeoutSeconds: *ring,
	})
	if err != nil {
		return err
	}
	fmt.Printf("内線 %s を登録しました（SIP ユーザー %s、エンドポイント PJSIP/%s）。\n", *number, *sipUser, *endpoint)
	return nil
}

func enrollCode(args []string) error {
	fs := flag.NewFlagSet("enroll-code", flag.ExitOnError)
	extension := fs.String("extension", "", "内線番号")
	_ = fs.Parse(args)
	if *extension == "" {
		return errors.New("--extension is required")
	}
	ctx, cfg, db, err := adminContext()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.GetExtension(ctx, *extension); err != nil {
		return fmt.Errorf("内線 %s は未登録です。先に extension set を実行してください", *extension)
	}
	code := secret.NewEnrollmentCode()
	expires := time.Now().Add(cfg.EnrollmentCodeTTL)
	if err := db.CreateEnrollmentCode(ctx, store.EnrollmentCode{CodeHash: secret.Hash(code), Extension: *extension, ExpiresAt: expires}); err != nil {
		return err
	}
	link := cfg.PublicURL + "/enroll?code=" + code
	fmt.Printf("内線 %s の登録コードを発行しました（%s まで・1回限り）\n\n", *extension, expires.Local().Format("15:04"))
	fmt.Printf("  設定URL : %s\n", cfg.PublicURL)
	fmt.Printf("  登録コード: %s\n\n", secret.FormatCode(code))
	fmt.Println("アプリの「QRコードを読み取る」で次の QR を読み取ることもできます（SIP パスワードは含みません）:")
	fmt.Println()
	if art, err := qr.Terminal(link); err == nil {
		fmt.Println(art)
	}
	return nil
}

func devices() error {
	ctx, _, db, err := adminContext()
	if err != nil {
		return err
	}
	defer db.Close()
	list, err := db.ListDevices(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%-40s %-6s %-8s %-10s %-6s %-8s %s\n", "端末ID", "内線", "状態", "APNs", "着信", "VoIP", "名前")
	for _, d := range list {
		state := "有効"
		if d.RevokedAt != nil {
			state = "失効"
		}
		enabled := "受ける"
		if !d.Enabled {
			enabled = "停止"
		}
		voip := "未登録"
		if d.VoIPToken != "" {
			voip = "登録済"
		}
		fmt.Printf("%-40s %-6s %-8s %-10s %-6s %-8s %s\n", d.ID, d.Extension, state, d.PushEnvironment, enabled, voip, d.Name)
	}
	return nil
}

func revokeDevice(args []string) error {
	fs := flag.NewFlagSet("revoke-device", flag.ExitOnError)
	id := fs.String("device", "", "端末ID")
	_ = fs.Parse(args)
	if *id == "" {
		return errors.New("--device is required")
	}
	ctx, _, db, err := adminContext()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.RevokeDevice(ctx, *id, time.Now()); err != nil {
		return err
	}
	fmt.Printf("端末 %s を失効しました。\n", *id)
	return nil
}

func checkARI(args []string) error {
	fs := flag.NewFlagSet("check-ari", flag.ExitOnError)
	extension := fs.String("extension", "", "Contact を確認する PJSIP エンドポイント")
	_ = fs.Parse(args)
	cfg, err := config.Load(false)
	if err != nil {
		return err
	}
	if cfg.ARIURL == "" {
		return errors.New("ARI_URL is required")
	}
	client := ari.New(cfg.ARIURL, cfg.ARIUser, cfg.ARIPassword, cfg.ARIApp, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("ARI に接続できません: %w", err)
	}
	fmt.Println("ARI に接続できました:", cfg.ARIURL)
	if *extension != "" {
		v, err := client.GetGlobal(ctx, fmt.Sprintf("PJSIP_DIAL_CONTACTS(%s)", *extension))
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Printf("PJSIP/%s の Contact: なし（アプリ休止中は正常）\n", *extension)
		} else {
			fmt.Printf("PJSIP/%s の Contact: %s\n", *extension, v)
		}
	}
	return nil
}
