package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nemnet-lab/MyVoIP-Server/internal/apns"
	"github.com/nemnet-lab/MyVoIP-Server/internal/calls"
	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

type nopARI struct{}

func (nopARI) Ring(context.Context, string) error                          { return nil }
func (nopARI) Answer(context.Context, string) error                        { return nil }
func (nopARI) Hangup(context.Context, string, string) error                { return nil }
func (nopARI) CreateChannel(context.Context, string, string, string) error { return nil }
func (nopARI) SetVariable(context.Context, string, string, string) error   { return nil }
func (nopARI) Dial(context.Context, string, string, int) error             { return nil }
func (nopARI) CreateBridge(context.Context, string) error                  { return nil }
func (nopARI) AddToBridge(context.Context, string, ...string) error        { return nil }
func (nopARI) DestroyBridge(context.Context, string) error                 { return nil }
func (nopARI) GetGlobal(context.Context, string) (string, error)           { return "", nil }
func (nopARI) Play(context.Context, string, string, string) error          { return nil }
func (nopARI) Record(context.Context, string, string, int) error           { return nil }
func (nopARI) DeleteStoredRecording(context.Context, string) error         { return nil }

type nopPusher struct{}

func (nopPusher) SendVoIP(context.Context, string, string, apns.VoIPPayload) error { return nil }
func (nopPusher) SendAlert(context.Context, string, string, string, string) error  { return nil }

type env struct {
	t     *testing.T
	srv   *httptest.Server
	store *store.Memory
	box   *secret.Box
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := store.NewMemory()
	box, err := secret.NewBox("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctrl := calls.New(st, nopARI{}, nopPusher{}, logger, calls.Options{})
	s := New(st, ctrl, box, Config{
		SIPDomain: "pbx.example.jp", SIPProxyPort: 5060, SIPTransport: "udp", SIPSRTP: "optional", BundleID: "net.nemnet-lab.myvoip",
		AccessTokenTTL: time.Hour, RefreshTokenTTL: 24 * time.Hour, RingTimeout: 30 * time.Second,
	}, logger)
	_ = st.UpsertExtension(context.Background(), store.Extension{
		Number: "201", DisplayName: "受付", SIPUsername: "201", SIPAuthUsername: "201",
		SIPPasswordEnc: box.Seal("sip-secret"), Endpoint: "201", RingTimeoutSeconds: 30,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, store: st, box: box}
}

func (e *env) code() string {
	code := secret.NewEnrollmentCode()
	_ = e.store.CreateEnrollmentCode(context.Background(), store.EnrollmentCode{CodeHash: secret.Hash(code), Extension: "201", ExpiresAt: time.Now().Add(10 * time.Minute)})
	return code
}

func (e *env) do(method, path, token string, body any) (*http.Response, map[string]any) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (e *env) enroll() map[string]any {
	code := e.code()
	resp, out := e.do("POST", "/v1/enroll", "", map[string]any{
		"code": secret.FormatCode(strings.ToLower(code)), "device_name": "iPhone", "platform": "ios", "app_version": "0.1.0", "push_environment": "sandbox",
	})
	if resp.StatusCode != 200 {
		e.t.Fatalf("enroll status %d %v", resp.StatusCode, out)
	}
	return out
}

func TestEnrollReturnsSIPSettingsAndCodeIsSingleUse(t *testing.T) {
	e := newEnv(t)
	code := e.code()
	body := map[string]any{"code": code, "device_name": "iPhone", "platform": "ios", "app_version": "0.1.0", "push_environment": "sandbox"}
	resp, out := e.do("POST", "/v1/enroll", "", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sip := out["sip"].(map[string]any)
	if sip["password"] != "sip-secret" || sip["transport"] != "udp" || sip["srtp"] != "optional" || sip["domain"] != "pbx.example.jp" {
		t.Fatalf("sip %v", sip)
	}
	if _, ok := sip["proxy_port"]; ok {
		t.Fatal("default port should be omitted")
	}
	if out["extension"].(map[string]any)["number"] != "201" {
		t.Fatal("extension missing")
	}
	resp, _ = e.do("POST", "/v1/enroll", "", body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("reuse should be 409, got %d", resp.StatusCode)
	}
	resp, _ = e.do("POST", "/v1/enroll", "", map[string]any{"code": "WRONG123", "push_environment": "sandbox"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code should be 401, got %d", resp.StatusCode)
	}
}

func TestReEnrollRevokesOldDevice(t *testing.T) {
	e := newEnv(t)
	first := e.enroll()
	second := e.enroll()
	resp, _ := e.do("GET", "/v1/device/status", first["access_token"].(string), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("old device token must be revoked (F-05)")
	}
	resp, _ = e.do("GET", "/v1/device/status", second["access_token"].(string), nil)
	if resp.StatusCode != 200 {
		t.Fatal("new device should work")
	}
}

func TestPushTokenStatusAvailabilityAndRefresh(t *testing.T) {
	e := newEnv(t)
	out := e.enroll()
	token := out["access_token"].(string)
	voip := strings.Repeat("ab", 32)

	resp, _ := e.do("PUT", "/v1/device/push-token", token, map[string]any{"voip_token": voip, "alert_token": nil, "environment": "sandbox", "bundle_id": "other.app"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal("wrong bundle must be rejected")
	}
	resp, _ = e.do("PUT", "/v1/device/push-token", token, map[string]any{"voip_token": voip, "alert_token": nil, "environment": "sandbox", "bundle_id": "net.nemnet-lab.myvoip"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("push token %d", resp.StatusCode)
	}
	_, status := e.do("GET", "/v1/device/status", token, nil)
	if status["voip_token_registered"] != true || status["enabled"] != true {
		t.Fatalf("status %v", status)
	}
	resp, _ = e.do("PUT", "/v1/device/availability", token, map[string]any{"enabled": false})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("availability")
	}
	_, status = e.do("GET", "/v1/device/status", token, nil)
	if status["enabled"] != false {
		t.Fatal("availability not stored")
	}

	refresh := out["refresh_token"].(string)
	resp, pair := e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	if resp.StatusCode != 200 || pair["access_token"] == "" {
		t.Fatal("refresh failed")
	}
	resp, _ = e.do("POST", "/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("refresh token must rotate")
	}
}

func TestLogoutRevokesDevice(t *testing.T) {
	e := newEnv(t)
	token := e.enroll()["access_token"].(string)
	resp, _ := e.do("DELETE", "/v1/device", token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("logout")
	}
	resp, _ = e.do("GET", "/v1/device/status", token, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("token must be revoked after logout")
	}
}

func TestHistoryFeedWithDeletes(t *testing.T) {
	e := newEnv(t)
	token := e.enroll()["access_token"].(string)
	ctx := context.Background()
	for i, id := range []string{"h-1", "h-2"} {
		d := 30
		_ = e.store.AddHistory(ctx, store.HistoryItem{ID: id, Extension: "201", CallID: "11111111-2222-3333-4444-55555555555" + string(rune('0'+i)), Direction: "incoming", Result: "answered", RemoteNumber: "0312345678", StartedAt: time.Now(), DurationSeconds: &d})
	}
	_, page := e.do("GET", "/v1/history", token, nil)
	if len(page["items"].([]any)) != 2 {
		t.Fatalf("page %v", page)
	}
	cursor := page["next_cursor"].(string)
	resp, _ := e.do("DELETE", "/v1/history/h-1", token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("delete")
	}
	_, page = e.do("GET", "/v1/history?cursor="+cursor, token, nil)
	if len(page["items"].([]any)) != 0 || page["deleted_ids"].([]any)[0] != "h-1" {
		t.Fatalf("delta %v", page)
	}
	_, page = e.do("GET", "/v1/history?cursor="+page["next_cursor"].(string), token, nil)
	if page["next_cursor"] != nil {
		t.Fatal("no more changes should return null cursor")
	}
}

func TestCallStatusNotVisibleToOtherDevice(t *testing.T) {
	e := newEnv(t)
	token := e.enroll()["access_token"].(string)
	resp, _ := e.do("GET", "/v1/calls/00000000-0000-4000-8000-000000000000", token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatal("unknown call should be 404")
	}
}

func TestEnrollRateLimit(t *testing.T) {
	e := newEnv(t)
	var last int
	for i := 0; i < 12; i++ {
		resp, _ := e.do("POST", "/v1/enroll", "", map[string]any{"code": "BADBAD22", "push_environment": "sandbox"})
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", last)
	}
}
