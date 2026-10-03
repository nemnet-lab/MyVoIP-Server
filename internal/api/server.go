// Package api は iPhone アプリ向けの HTTPS API（docs/api.md）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nemnet-lab/MyVoIP-Server/internal/calls"
	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

type Config struct {
	SIPDomain       string
	SIPProxyHost    string
	SIPProxyPort    int
	SIPTransport    string
	SIPSRTP         string
	BundleID        string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	RingTimeout     time.Duration
}

type Server struct {
	store store.Store
	calls *calls.Controller
	box   *secret.Box
	cfg   Config
	log   *slog.Logger

	limiter *rateLimiter
}

func New(st store.Store, ctrl *calls.Controller, box *secret.Box, cfg Config, log *slog.Logger) *Server {
	return &Server{store: st, calls: ctrl, box: box, cfg: cfg, log: log, limiter: newRateLimiter(10, time.Minute)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /v1/enroll", s.enroll)
	mux.HandleFunc("POST /v1/auth/refresh", s.refresh)
	mux.HandleFunc("PUT /v1/device/push-token", s.auth(s.updatePushToken))
	mux.HandleFunc("PUT /v1/device/availability", s.auth(s.setAvailability))
	mux.HandleFunc("GET /v1/device/status", s.auth(s.deviceStatus))
	mux.HandleFunc("POST /v1/device/test-call", s.auth(s.testCall))
	mux.HandleFunc("DELETE /v1/device", s.auth(s.deleteDevice))
	mux.HandleFunc("POST /v1/calls/{id}/ready", s.auth(s.ready))
	mux.HandleFunc("GET /v1/calls/{id}", s.auth(s.callStatus))
	mux.HandleFunc("POST /v1/calls/{id}/decline", s.auth(s.decline))
	mux.HandleFunc("GET /v1/calls/{id}/events", s.auth(s.callEvents))
	mux.HandleFunc("GET /v1/history", s.auth(s.history))
	mux.HandleFunc("DELETE /v1/history/{id}", s.auth(s.deleteHistory))
	mux.HandleFunc("DELETE /v1/history", s.auth(s.deleteAllHistory))
	return s.logRequests(mux)
}

// --- 共通

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	return dec.Decode(v)
}

func clientIP(r *http.Request) string {
	// Caddy の背後で動かすため X-Forwarded-For の先頭を使う
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap は WebSocket の Hijack を通すため。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		// 認証ヘッダー・トークンはログに出さない（02 §6）
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}

type ctxKey struct{}

func deviceFrom(ctx context.Context) store.Device {
	return ctx.Value(ctxKey{}).(store.Device)
}

// auth はアクセストークンを検証し、端末単位の権限で処理させる（02 §6）。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		t, err := s.store.GetToken(r.Context(), secret.Hash(token))
		if err != nil || t.Kind != store.TokenAccess || t.RevokedAt != nil || time.Now().After(t.ExpiresAt) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		device, err := s.store.GetDevice(r.Context(), t.DeviceID)
		if err != nil || device.RevokedAt != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, device)))
	}
}

// --- 登録・認証

type extensionJSON struct {
	Number      string `json:"number"`
	DisplayName string `json:"display_name,omitempty"`
}

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) issueTokens(ctx context.Context, deviceID string) (tokenPair, error) {
	access, refresh := secret.NewToken(), secret.NewToken()
	now := time.Now()
	if err := s.store.CreateToken(ctx, store.Token{Hash: secret.Hash(access), DeviceID: deviceID, Kind: store.TokenAccess, ExpiresAt: now.Add(s.cfg.AccessTokenTTL)}); err != nil {
		return tokenPair{}, err
	}
	if err := s.store.CreateToken(ctx, store.Token{Hash: secret.Hash(refresh), DeviceID: deviceID, Kind: store.TokenRefresh, ExpiresAt: now.Add(s.cfg.RefreshTokenTTL)}); err != nil {
		return tokenPair{}, err
	}
	return tokenPair{AccessToken: access, ExpiresIn: int(s.cfg.AccessTokenTTL.Seconds()), RefreshToken: refresh}, nil
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var req struct {
		Code            string `json:"code"`
		DeviceName      string `json:"device_name"`
		Platform        string `json:"platform"`
		AppVersion      string `json:"app_version"`
		PushEnvironment string `json:"push_environment"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.PushEnvironment != "sandbox" && req.PushEnvironment != "production" {
		writeError(w, http.StatusBadRequest, "bad_push_environment")
		return
	}
	ctx := r.Context()
	code, err := s.store.ConsumeEnrollmentCode(ctx, secret.Hash(secret.NormalizeCode(req.Code)), time.Now())
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrExpired):
		s.limiter.fail(ip)
		writeError(w, http.StatusUnauthorized, "invalid_code")
		return
	case errors.Is(err, store.ErrUsed):
		s.limiter.fail(ip)
		writeError(w, http.StatusConflict, "code_used")
		return
	case err != nil:
		s.log.Error("enroll", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	ext, err := s.store.GetExtension(ctx, code.Extension)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	password, err := s.box.Open(ext.SIPPasswordEnc)
	if err != nil {
		s.log.Error("enroll: decrypt sip password", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	device := store.Device{
		ID:              "d-" + secret.NewID(),
		Extension:       ext.Number,
		Name:            truncate(req.DeviceName, 100),
		Platform:        "ios",
		AppVersion:      truncate(req.AppVersion, 32),
		PushEnvironment: req.PushEnvironment,
	}
	// 同じ内線の旧端末の資格情報・Push 紐付けを失効させる（F-05）
	if err := s.store.CreateDevice(ctx, device, time.Now()); err != nil {
		s.log.Error("enroll: create device", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	tokens, err := s.issueTokens(ctx, device.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	s.log.Info("device enrolled", "device_id", device.ID, "extension", ext.Number, "push_environment", req.PushEnvironment)

	transport := s.cfg.SIPTransport
	if transport == "" {
		transport = "udp"
	}
	defaultPort := 5060
	if transport == "tls" {
		defaultPort = 5061
	}
	srtp := s.cfg.SIPSRTP
	if srtp == "" {
		srtp = "optional"
	}
	sip := map[string]any{
		"domain":        s.cfg.SIPDomain,
		"transport":     transport,
		"srtp":          srtp,
		"username":      ext.SIPUsername,
		"auth_username": ext.SIPAuthUsername,
		"password":      password,
	}
	if s.cfg.SIPProxyHost != "" && s.cfg.SIPProxyHost != s.cfg.SIPDomain {
		sip["proxy_host"] = s.cfg.SIPProxyHost
	}
	if s.cfg.SIPProxyPort != 0 && s.cfg.SIPProxyPort != defaultPort {
		sip["proxy_port"] = s.cfg.SIPProxyPort
	}
	ring := ext.RingTimeoutSeconds
	if ring < 15 || ring > 60 {
		ring = int(s.cfg.RingTimeout.Seconds())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":               device.ID,
		"access_token":            tokens.AccessToken,
		"access_token_expires_in": tokens.ExpiresIn,
		"refresh_token":           tokens.RefreshToken,
		"extension":               extensionJSON{Number: ext.Number, DisplayName: ext.DisplayName},
		"sip":                     sip,
		"policy":                  map[string]int{"ring_timeout_seconds": ring},
	})
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := readJSON(r, &req); err != nil || req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	ctx := r.Context()
	hash := secret.Hash(req.RefreshToken)
	t, err := s.store.GetToken(ctx, hash)
	if err != nil || t.Kind != store.TokenRefresh || t.RevokedAt != nil || time.Now().After(t.ExpiresAt) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	device, err := s.store.GetDevice(ctx, t.DeviceID)
	if err != nil || device.RevokedAt != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// 更新トークンはローテーションする
	_ = s.store.RevokeToken(ctx, hash, time.Now())
	tokens, err := s.issueTokens(ctx, device.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// --- 端末

func (s *Server) updatePushToken(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	var req struct {
		VoIPToken   string  `json:"voip_token"`
		AlertToken  *string `json:"alert_token"`
		Environment string  `json:"environment"`
		BundleID    string  `json:"bundle_id"`
	}
	if err := readJSON(r, &req); err != nil || !isHexToken(req.VoIPToken) {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	// 開発・本番トークンと送信先を分離する（01 §7-5）
	if req.BundleID != s.cfg.BundleID || (req.Environment != "sandbox" && req.Environment != "production") {
		writeError(w, http.StatusBadRequest, "bad_bundle_or_environment")
		return
	}
	alert := ""
	if req.AlertToken != nil && isHexToken(*req.AlertToken) {
		alert = *req.AlertToken
	}
	if err := s.store.UpdateDevicePush(r.Context(), device.ID, req.VoIPToken, alert, req.Environment); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	s.log.Info("push token updated", "device_id", device.ID, "voip", secret.ShortID(req.VoIPToken), "alert", alert != "", "environment", req.Environment)
	w.WriteHeader(http.StatusNoContent)
}

func isHexToken(s string) bool {
	if len(s) < 32 || len(s) > 200 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func (s *Server) setAvailability(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if err := s.store.SetDeviceEnabled(r.Context(), device.ID, req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	s.log.Info("availability changed", "device_id", device.ID, "enabled", req.Enabled)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deviceStatus(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	ext, _ := s.store.GetExtension(r.Context(), device.Extension)
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":               device.Enabled,
		"voip_token_registered": device.VoIPToken != "",
		"extension":             extensionJSON{Number: device.Extension, DisplayName: ext.DisplayName},
		"server_time":           float64(time.Now().UnixMilli()) / 1000,
	})
}

func (s *Server) testCall(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	id, err := s.calls.StartTestCall(r.Context(), device)
	switch {
	case errors.Is(err, calls.ErrBusy):
		writeError(w, http.StatusConflict, "busy")
	case errors.Is(err, calls.ErrNoPushToken):
		writeError(w, http.StatusConflict, "push_token_missing")
	case err != nil:
		s.log.Error("test call", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"call_id": id})
	}
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	if err := s.store.RevokeDevice(r.Context(), device.ID, time.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	s.log.Info("device logged out", "device_id", device.ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- 呼

func (s *Server) writeCallResult(w http.ResponseWriter, status calls.Status, err error) {
	if errors.Is(err, calls.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RegistrationID string `json:"registration_id"`
	}
	if err := readJSON(r, &req); err != nil || req.RegistrationID == "" || len(req.RegistrationID) > 64 {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	device := deviceFrom(r.Context())
	status, err := s.calls.Ready(r.Context(), device.ID, strings.ToLower(r.PathValue("id")), req.RegistrationID)
	s.writeCallResult(w, status, err)
}

func (s *Server) callStatus(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	status, err := s.calls.Status(r.Context(), device.ID, strings.ToLower(r.PathValue("id")))
	s.writeCallResult(w, status, err)
}

func (s *Server) decline(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	_ = readJSON(r, &req)
	device := deviceFrom(r.Context())
	status, err := s.calls.Decline(r.Context(), device.ID, strings.ToLower(r.PathValue("id")), req.Reason)
	s.writeCallResult(w, status, err)
}

// callEvents は着信処理中・通話中だけ使う WebSocket（F-14）。
func (s *Server) callEvents(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	callID := strings.ToLower(r.PathValue("id"))
	initial, ch, cancel, err := s.calls.Subscribe(r.Context(), device.ID, callID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	defer cancel()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(r.Context())
	send := func(st calls.Status) bool {
		data, _ := json.Marshal(st)
		wctx, c := context.WithTimeout(ctx, 5*time.Second)
		defer c()
		return conn.Write(wctx, websocket.MessageText, data) == nil
	}
	if !send(initial) || initial.State == calls.StateEnded || ch == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	// 呼の最大寿命（呼出60秒＋通話）を超えて張り続けない
	limit := time.NewTimer(4 * time.Hour)
	defer limit.Stop()
	for {
		select {
		case st := <-ch:
			if !send(st) {
				return
			}
			if st.State == calls.StateEnded {
				conn.Close(websocket.StatusNormalClosure, "")
				return
			}
		case <-ctx.Done():
			return
		case <-limit.C:
			conn.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

// --- 履歴

type historyJSON struct {
	ID              string  `json:"id"`
	CallID          *string `json:"call_id"`
	Direction       string  `json:"direction"`
	Result          string  `json:"result"`
	RemoteNumber    string  `json:"remote_number"`
	RemoteName      *string `json:"remote_name"`
	StartedAt       float64 `json:"started_at"`
	DurationSeconds *int    `json:"duration_seconds"`
	Detail          *string `json:"detail"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	var cursor int64
	if c := r.URL.Query().Get("cursor"); c != "" {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_cursor")
			return
		}
		cursor = n
	}
	const limit = 200
	rows, err := s.store.ListHistory(r.Context(), device.Extension, cursor, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	items := []historyJSON{}
	deleted := []string{}
	next := cursor
	for _, h := range rows {
		next = h.Seq
		if h.DeletedAt != nil {
			deleted = append(deleted, h.ID)
			continue
		}
		items = append(items, historyJSON{
			ID: h.ID, CallID: optional(h.CallID), Direction: h.Direction, Result: h.Result,
			RemoteNumber: h.RemoteNumber, RemoteName: optional(h.RemoteName),
			StartedAt: float64(h.StartedAt.UnixMilli()) / 1000, DurationSeconds: h.DurationSeconds, Detail: optional(h.Detail),
		})
	}
	resp := map[string]any{"items": items, "deleted_ids": deleted, "next_cursor": nil}
	// 続きがある場合も、ない場合も次回の差分取得の起点を返す
	if next != cursor || len(rows) > 0 {
		resp["next_cursor"] = strconv.FormatInt(next, 10)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteHistory(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	err := s.store.DeleteHistory(r.Context(), device.Extension, r.PathValue("id"), time.Now())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAllHistory(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	if err := s.store.DeleteAllHistory(r.Context(), device.Extension, time.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- 登録コードの試行回数制限（02 §6）

type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *rateLimiter) prune(key string, now time.Time) []time.Time {
	list := l.hits[key]
	kept := list[:0]
	for _, t := range list {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, time.Now())) < l.max
}

func (l *rateLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[key] = append(l.prune(key, time.Now()), time.Now())
}
