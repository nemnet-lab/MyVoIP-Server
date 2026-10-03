// Package ari は Asterisk REST Interface（MikoPBX 同梱 Asterisk）のクライアント。
// ARI は管理ネットワーク内でのみ使い、インターネットに公開しない（03 §4）。
package ari

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

var ErrNotFound = errors.New("ari: not found")

type Caller struct {
	Name   string `json:"name"`
	Number string `json:"number"`
}

type Dialplan struct {
	Context string `json:"context"`
	Exten   string `json:"exten"`
}

type Channel struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	State    string   `json:"state"`
	Caller   Caller   `json:"caller"`
	Dialplan Dialplan `json:"dialplan"`
}

type Playback struct {
	ID        string `json:"id"`
	TargetURI string `json:"target_uri"`
}

type Recording struct {
	Name      string `json:"name"`
	TargetURI string `json:"target_uri"`
}

// Event は必要なフィールドだけを持つ ARI イベント。
type Event struct {
	Type        string     `json:"type"`
	Application string     `json:"application"`
	Args        []string   `json:"args"`
	Channel     *Channel   `json:"channel"`
	Peer        *Channel   `json:"peer"`
	Cause       int        `json:"cause"`
	CauseTxt    string     `json:"cause_txt"`
	DialStatus  string     `json:"dialstatus"`
	Playback    *Playback  `json:"playback"`
	Recording   *Recording `json:"recording"`
}

type Client struct {
	base     string
	user     string
	password string
	app      string
	http     *http.Client
	log      *slog.Logger
}

func New(baseURL, user, password, app string, log *slog.Logger) *Client {
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		user:     user,
		password: password,
		app:      app,
		http:     &http.Client{Timeout: 8 * time.Second},
		log:      log,
	}
}

func (c *Client) App() string { return c.app }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.password)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ari %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ari %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

// Ring は発信側へ呼出中を返す（最終応答はまだしない: 03 §5.2）。
func (c *Client) Ring(ctx context.Context, channelID string) error {
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/ring", nil, nil)
}

func (c *Client) Answer(ctx context.Context, channelID string) error {
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/answer", nil, nil)
}

// Hangup の reason は normal / busy / congestion / no_answer など。
func (c *Client) Hangup(ctx context.Context, channelID, reason string) error {
	q := url.Values{}
	if reason != "" {
		q.Set("reason", reason)
	}
	err := c.do(ctx, http.MethodDelete, "/channels/"+url.PathEscape(channelID), q, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// CreateChannel は発呼前のチャネルを作る。応答後に Stasis(app, appArgs) へ入る。
func (c *Client) CreateChannel(ctx context.Context, endpoint, appArgs, channelID string) error {
	q := url.Values{"endpoint": {endpoint}, "app": {c.app}, "appArgs": {appArgs}, "channelId": {channelID}}
	return c.do(ctx, http.MethodPost, "/channels/create", q, nil)
}

func (c *Client) SetVariable(ctx context.Context, channelID, name, value string) error {
	q := url.Values{"variable": {name}, "value": {value}}
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/variable", q, nil)
}

func (c *Client) Dial(ctx context.Context, channelID, callerChannelID string, timeoutSeconds int) error {
	q := url.Values{"timeout": {fmt.Sprint(timeoutSeconds)}}
	if callerChannelID != "" {
		q.Set("caller", callerChannelID)
	}
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/dial", q, nil)
}

func (c *Client) CreateBridge(ctx context.Context, bridgeID string) error {
	q := url.Values{"type": {"mixing"}, "bridgeId": {bridgeID}, "name": {bridgeID}}
	return c.do(ctx, http.MethodPost, "/bridges", q, nil)
}

func (c *Client) AddToBridge(ctx context.Context, bridgeID string, channels ...string) error {
	q := url.Values{"channel": {strings.Join(channels, ",")}}
	return c.do(ctx, http.MethodPost, "/bridges/"+url.PathEscape(bridgeID)+"/addChannel", q, nil)
}

func (c *Client) DestroyBridge(ctx context.Context, bridgeID string) error {
	err := c.do(ctx, http.MethodDelete, "/bridges/"+url.PathEscape(bridgeID), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// GetGlobal は関数を含むグローバル変数を評価する（例 PJSIP_DIAL_CONTACTS(201)）。
func (c *Client) GetGlobal(ctx context.Context, expr string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := c.do(ctx, http.MethodGet, "/asterisk/variable", url.Values{"variable": {expr}}, &out)
	return out.Value, err
}

func (c *Client) Play(ctx context.Context, channelID, media, playbackID string) error {
	q := url.Values{"media": {media}}
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/play/"+url.PathEscape(playbackID), q, nil)
}

func (c *Client) Record(ctx context.Context, channelID, name string, maxSeconds int) error {
	q := url.Values{"name": {name}, "format": {"wav"}, "maxDurationSeconds": {fmt.Sprint(maxSeconds)},
		"beep": {"true"}, "ifExists": {"overwrite"}, "terminateOn": {"none"}}
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/record", q, nil)
}

func (c *Client) DeleteStoredRecording(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/recordings/stored/"+url.PathEscape(name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Ping は接続確認（/asterisk/info）。
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/asterisk/info", nil, nil)
}

// Run は ARI の WebSocket に接続し、切断されたら待って再接続する。ctx が終わるまで戻らない。
func (c *Client) Run(ctx context.Context, handle func(Event)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.listen(ctx, handle)
		if ctx.Err() != nil {
			return
		}
		c.log.Warn("ARI events disconnected", "error", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) listen(ctx context.Context, handle func(Event)) error {
	u, err := url.Parse(c.base + "/events")
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.RawQuery = url.Values{"app": {c.app}, "subscribeAll": {"false"}}.Encode()
	header := http.Header{}
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.SetBasicAuth(c.user, c.password)
	header.Set("Authorization", req.Header.Get("Authorization"))

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{HTTPHeader: header})
	cancel()
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	c.log.Info("ARI events connected", "app", c.app)

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			c.log.Warn("ARI event decode failed", "error", err)
			continue
		}
		handle(ev)
	}
}
