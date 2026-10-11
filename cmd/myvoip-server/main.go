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
//	myvoip-server relay sync                  中継用 Asterisk の設定を書き出して再読み込みさせる（中継モード）
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
	"github.com/nemnet-lab/MyVoIP-Server/internal/relay"
	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
	"github.com/nemnet-lab/MyVoIP-Server/internal/web"
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
	case "relay":
		err = relayCmd(os.Args[2:])
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
  myvoip-server extension set --number 201 [--display-name 名前] [--sip-user 201] [--auth-user 201] [--endpoint 201] [--ring-timeout 30] [--pool]
      （SIP パスワードは標準入力または環境変数 SIP_PASSWORD で渡す）
  myvoip-server extension list
  myvoip-server enroll-code --extension 201
  myvoip-server devices
  myvoip-server revoke-device --device d-xxxx
  myvoip-server check-ari [--extension 201]
  myvoip-server gen-secret
  myvoip-server relay sync`)
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
	pusher := apns.New(key, cfg.APNSKeyID, cfg.APNSTeamID, cfg.APNSBundleID).WithEndpoint(cfg.APNSEndpoint)
	ariClient := ari.New(cfg.ARIURL, cfg.ARIUser, cfg.ARIPassword, cfg.ARIApp, log)
	// PJSIP の登録先の変化（ContactStatusChange）を受け取り、端末への発呼先の特定に使う
	ariClient.SubscribeOnConnect("endpoint:PJSIP")
	controller := calls.New(db, ariClient, pusher, log, calls.Options{
		RingTimeout: cfg.RingTimeout, TestSound: cfg.TestCallSound, DialExtensionNumber: cfg.RelayEnabled,
	})
	if cfg.RelayEnabled {
		// 中継用 Asterisk の設定を DB から書き出す。Asterisk の起動待ちのため、反映できるまで再試行する
		go syncRelayUntilDone(ctx, db, box, cfg, ariClient, log)
	}

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

	var webHandler http.Handler
	if cfg.Web.Enabled {
		var syncer web.Syncer
		if cfg.RelayEnabled {
			syncer = func(ctx context.Context) error {
				return relay.Sync(ctx, db, box, relayOptions(cfg), cfg.RelayConfigDir, ariClient)
			}
		}
		webServer, err := web.New(db, box, web.Config{
			PublicURL: cfg.PublicURL, EnrollmentCodeTTL: cfg.EnrollmentCodeTTL, Relay: cfg.RelayEnabled, Web: cfg.Web,
		}, syncer, log)
		if err != nil {
			return err
		}
		webHandler = webServer.Handler()
		log.Info("web console enabled", "url", cfg.PublicURL+"/", "redirect_uri", cfg.PublicURL+"/auth/discord/callback",
			"guild_restricted", cfg.Web.DiscordGuildID != "", "admins", len(cfg.Web.AdminIDs))
	}

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
		AllowedClients:  cfg.AllowedClients,
		Relay:           cfg.RelayEnabled,
		Web:             webHandler,
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
		"sip_transport", cfg.SIPTransport, "sip_port", cfg.SIPProxyPort, "srtp", cfg.SIPSRTP,
		"allowed_clients", fmt.Sprint(cfg.AllowedClients))
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func relayOptions(cfg config.Config) relay.Options {
	return relay.Options{
		UpstreamHost:   cfg.UpstreamSIPHost,
		UpstreamPort:   cfg.UpstreamSIPPort,
		AllowedClients: cfg.AllowedClients,
		SRTP:           cfg.SIPSRTP,
	}
}

func syncRelayUntilDone(ctx context.Context, db store.Store, box *secret.Box, cfg config.Config, reloader relay.Reloader, log *slog.Logger) {
	delay := 2 * time.Second
	for ctx.Err() == nil {
		syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := relay.Sync(syncCtx, db, box, relayOptions(cfg), cfg.RelayConfigDir, reloader)
		cancel()
		if err == nil {
			log.Info("relay configuration applied", "upstream", fmt.Sprintf("%s:%d", cfg.UpstreamSIPHost, cfg.UpstreamSIPPort))
			// エンドポイントができたので、登録先のイベントの購読をすぐ試す（失敗しても接続側で再試行する）
			if client, ok := reloader.(*ari.Client); ok {
				subCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := client.Subscribe(subCtx, "endpoint:PJSIP"); err == nil {
					log.Info("ARI subscribed", "source", "endpoint:PJSIP")
				}
				cancel()
			}
			return
		}
		log.Warn("relay configuration not applied yet; retrying", "error", err, "retry_in", delay.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
}

// relayCmd は中継用 Asterisk の設定を書き出して反映する。
func relayCmd(args []string) error {
	if len(args) == 0 || args[0] != "sync" {
		return errors.New("relay sync")
	}
	ctx, cfg, db, err := adminContext()
	if err != nil {
		return err
	}
	defer db.Close()
	return relaySync(ctx, cfg, db)
}

func relaySync(ctx context.Context, cfg config.Config, db store.Store) error {
	if !cfg.RelayEnabled {
		return errors.New("RELAY_ENABLED=true ではありません")
	}
	box, err := secret.NewBox(cfg.SecretKey)
	if err != nil {
		return err
	}
	client := ari.New(cfg.ARIURL, cfg.ARIUser, cfg.ARIPassword, cfg.ARIApp, slog.Default())
	syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := relay.Sync(syncCtx, db, box, relayOptions(cfg), cfg.RelayConfigDir, client); err != nil {
		return err
	}
	fmt.Printf("中継用 Asterisk の設定を反映しました（MikoPBX %s:%d へ登録）。\n", cfg.UpstreamSIPHost, cfg.UpstreamSIPPort)
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
		users, err := db.ListUsers(ctx)
		if err != nil {
			return err
		}
		names := map[string]string{}
		for _, u := range users {
			names[u.ID] = u.DisplayName
		}
		fmt.Printf("%-8s %-20s %-10s %-10s %-6s %-8s %s\n", "内線", "表示名", "SIPユーザー", "エンドポイント", "呼出秒", "割当用", "利用者")
		for _, e := range list {
			pool := "-"
			if e.Pool {
				pool = "はい"
			}
			owner := "-"
			if e.OwnerUserID != "" {
				owner = names[e.OwnerUserID] + "（" + e.Label + "）"
			}
			fmt.Printf("%-8s %-20s %-10s %-10s %-6d %-8s %s\n", e.Number, e.DisplayName, e.SIPUsername, e.Endpoint, e.RingTimeoutSeconds, pool, owner)
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
	pool := fs.Bool("pool", false, "Web 管理画面で利用者に割り当てる内線にする")
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
	if *pool {
		if err := db.SetExtensionPool(ctx, *number, true); err != nil {
			return err
		}
	}
	fmt.Printf("内線 %s を登録しました（SIP ユーザー %s、エンドポイント PJSIP/%s）。\n", *number, *sipUser, *endpoint)
	if *pool {
		fmt.Println("Web 管理画面で利用者に割り当てる内線にしました（割り当てるまで MikoPBX には登録しません）。")
	}
	if cfg.RelayEnabled {
		// 中継用 Asterisk が MikoPBX にこの内線として登録するよう、設定を反映する
		if err := relaySync(ctx, cfg, db); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 中継用 Asterisk への反映に失敗しました（あとで relay sync を実行してください）: %v\n", err)
		}
	}
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
		state, err := client.EndpointState(ctx, "PJSIP", *extension)
		if err != nil {
			return fmt.Errorf("PJSIP/%s を取得できません（エンドポイント名を確認してください）: %w", *extension, err)
		}
		switch state {
		case "online":
			fmt.Printf("PJSIP/%s: online（到達可能な登録あり）\n", *extension)
		case "offline":
			fmt.Printf("PJSIP/%s: offline（登録なし。アプリ休止中は正常）\n", *extension)
		default:
			fmt.Printf("PJSIP/%s: %s\n", *extension, state)
		}
	}
	return nil
}
