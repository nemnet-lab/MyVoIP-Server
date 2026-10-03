// Package calls は MikoPBX からの着信を保持し、Push で起こした端末へつなぐ呼制御（03 §5）。
package calls

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nemnet-lab/MyVoIP-Server/internal/apns"
	"github.com/nemnet-lab/MyVoIP-Server/internal/ari"
	"github.com/nemnet-lab/MyVoIP-Server/internal/secret"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

// 呼状態（03 §6）。ended は終端。
const (
	StateCreated      = "created"
	StatePushing      = "pushing"
	StateWaitingReady = "waiting_ready"
	StateInviting     = "inviting"
	StateRinging      = "ringing"
	StateConnected    = "connected"
	StateEnded        = "ended"
)

// 終了理由（docs/api.md）。
const (
	ReasonCallerCancelled   = "caller_cancelled"
	ReasonTimeout           = "timeout"
	ReasonDeclined          = "declined"
	ReasonBusy              = "busy"
	ReasonCompleted         = "completed"
	ReasonPaused            = "paused"
	ReasonServerError       = "server_error"
	ReasonDeviceUnreachable = "device_unreachable"
)

var (
	ErrNotFound    = errors.New("call not found")
	ErrBusy        = errors.New("device busy")
	ErrNoPushToken = errors.New("device has no push token")
)

// ARI は呼制御に使う ARI 操作。
type ARI interface {
	Ring(ctx context.Context, channelID string) error
	Answer(ctx context.Context, channelID string) error
	Hangup(ctx context.Context, channelID, reason string) error
	CreateChannel(ctx context.Context, endpoint, appArgs, channelID string) error
	SetVariable(ctx context.Context, channelID, name, value string) error
	Dial(ctx context.Context, channelID, callerChannelID string, timeoutSeconds int) error
	CreateBridge(ctx context.Context, bridgeID string) error
	AddToBridge(ctx context.Context, bridgeID string, channels ...string) error
	DestroyBridge(ctx context.Context, bridgeID string) error
	GetGlobal(ctx context.Context, expr string) (string, error)
	Play(ctx context.Context, channelID, media, playbackID string) error
	Record(ctx context.Context, channelID, name string, maxSeconds int) error
	DeleteStoredRecording(ctx context.Context, name string) error
}

// Pusher は APNs 送信。
type Pusher interface {
	SendVoIP(ctx context.Context, token, environment string, p apns.VoIPPayload) error
	SendAlert(ctx context.Context, token, environment, title, message string) error
}

// Status は API・WebSocket で返す呼状態（iOS の CallStatus）。
type Status struct {
	ID           string  `json:"id"`
	State        string  `json:"state"`
	EndReason    *string `json:"end_reason"`
	ExpiresAt    float64 `json:"expires_at"`
	CallerNumber string  `json:"caller_number"`
	CallerName   string  `json:"caller_name"`
}

type Options struct {
	RingTimeout time.Duration
	TestSound   string
	// ContactWait は ready 後に Contact の登録を待つ上限
	ContactWait time.Duration
}

type Controller struct {
	store store.Store
	ari   ARI
	push  Pusher
	log   *slog.Logger
	opts  Options

	mu        sync.Mutex
	calls     map[string]*active
	byChannel map[string]string
	subs      map[string]map[chan Status]struct{}
}

type active struct {
	call           store.Call
	device         store.Device
	endpoint       string
	registrationID string
	bridgeID       string
	timer          *time.Timer
	testTimer      *time.Timer
}

func New(st store.Store, a ARI, p Pusher, log *slog.Logger, opts Options) *Controller {
	if opts.RingTimeout == 0 {
		opts.RingTimeout = 30 * time.Second
	}
	if opts.ContactWait == 0 {
		opts.ContactWait = 3 * time.Second
	}
	if opts.TestSound == "" {
		opts.TestSound = "sound:hello-world"
	}
	return &Controller{
		store: st, ari: a, push: p, log: log, opts: opts,
		calls:     map[string]*active{},
		byChannel: map[string]string{},
		subs:      map[string]map[chan Status]struct{}{},
	}
}

func ctxTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// run は副作用をロック外で順に実行する。
func run(effects []func()) {
	for _, f := range effects {
		f()
	}
}

// --- ARI イベント

func (c *Controller) HandleEvent(ev ari.Event) {
	switch ev.Type {
	case "StasisStart":
		if ev.Channel == nil {
			return
		}
		if len(ev.Args) >= 2 && ev.Args[0] == "device" {
			c.onDeviceAnswered(ev.Args[1], ev.Channel.ID)
			return
		}
		ext := ev.Channel.Dialplan.Exten
		if len(ev.Args) >= 1 && ev.Args[0] != "" {
			ext = ev.Args[0]
		}
		c.onIncoming(*ev.Channel, ext)
	case "StasisEnd", "ChannelDestroyed":
		if ev.Channel != nil {
			c.onChannelGone(ev.Channel.ID, ev.Cause, ev.Type == "ChannelDestroyed")
		}
	case "ChannelStateChange":
		if ev.Channel != nil && ev.Channel.State == "Ringing" {
			c.onDeviceRinging(ev.Channel.ID)
		}
	case "PlaybackFinished":
		if ev.Playback != nil {
			c.onPlaybackFinished(ev.Playback.ID)
		}
	case "RecordingFinished", "RecordingFailed":
		if ev.Recording != nil {
			c.onRecordingFinished(ev.Recording.Name)
		}
	}
}

// onIncoming は MikoPBX のダイヤルプランから Stasis に入った発信側の呼（03 §5.1）。
func (c *Controller) onIncoming(ch ari.Channel, extension string) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	log := c.log.With("extension", extension, "caller_channel", ch.ID)

	ext, err := c.store.GetExtension(ctx, extension)
	if err != nil {
		log.Warn("incoming call for unknown extension")
		_ = c.ari.Hangup(ctx, ch.ID, "no_answer")
		return
	}
	device, err := c.store.ActiveDeviceForExtension(ctx, extension)
	if err != nil {
		log.Warn("incoming call but no enrolled device")
		_ = c.ari.Hangup(ctx, ch.ID, "no_answer")
		return
	}

	now := time.Now()
	ringTimeout := c.opts.RingTimeout
	if ext.RingTimeoutSeconds >= 15 && ext.RingTimeoutSeconds <= 60 {
		ringTimeout = time.Duration(ext.RingTimeoutSeconds) * time.Second
	}
	call := store.Call{
		ID:            secret.NewID(),
		DeviceID:      device.ID,
		Extension:     extension,
		CallerNumber:  ch.Caller.Number,
		CallerName:    ch.Caller.Name,
		State:         StateCreated,
		CallerChannel: ch.ID,
		CreatedAt:     now,
		ExpiresAt:     now.Add(ringTimeout),
	}
	log = log.With("call_id", call.ID)

	c.mu.Lock()
	// 通話中・着信停止中・Push 先なしはサーバーで早期に決定する（03 §5 末尾, F-16）
	var reject, hangupReason string
	switch {
	case c.deviceBusyLocked(device.ID):
		reject, hangupReason = ReasonBusy, "busy"
	case !device.Enabled:
		reject, hangupReason = ReasonPaused, "busy"
	case device.VoIPToken == "":
		reject, hangupReason = ReasonDeviceUnreachable, "no_answer"
	}
	if reject != "" {
		call.State = StateEnded
		call.EndReason = reject
		call.EndedAt = &now
		_ = c.store.SaveCall(ctx, call)
		c.addHistoryLocked(ctx, call)
		c.mu.Unlock()
		log.Info("incoming call rejected early", "reason", reject)
		_ = c.ari.Hangup(ctx, ch.ID, hangupReason)
		return
	}
	call.State = StatePushing
	a := &active{call: call, device: device, endpoint: ext.Endpoint}
	c.calls[call.ID] = a
	c.byChannel[ch.ID] = call.ID
	_ = c.store.SaveCall(ctx, call)
	a.timer = time.AfterFunc(time.Until(call.ExpiresAt), func() { c.onTimeout(call.ID) })
	c.mu.Unlock()

	log.Info("incoming call accepted", "caller", ch.Caller.Number)
	// 発信側には呼出中を返し、まだ最終応答しない
	if err := c.ari.Ring(ctx, ch.ID); err != nil {
		log.Warn("ring indication failed", "error", err)
	}
	go c.sendPush(call.ID)
}

// sendPush は VoIP Push を1回送る（無制限再送はしない: 03 §5.3）。
func (c *Controller) sendPush(callID string) {
	c.mu.Lock()
	a := c.calls[callID]
	if a == nil {
		c.mu.Unlock()
		return
	}
	call, device := a.call, a.device
	c.mu.Unlock()

	ctx, cancel := ctxTimeout()
	defer cancel()
	started := time.Now()
	payload := apns.VoIPPayload{
		CallID:       call.ID,
		DeviceID:     device.ID,
		SentAt:       float64(time.Now().UnixMilli()) / 1000,
		ExpiresAt:    float64(call.ExpiresAt.UnixMilli()) / 1000,
		CallerNumber: call.CallerNumber,
		CallerName:   call.CallerName,
	}
	err := c.push.SendVoIP(ctx, device.VoIPToken, device.PushEnvironment, payload)
	log := c.log.With("call_id", callID, "device_id", device.ID)

	c.mu.Lock()
	a = c.calls[callID]
	if a == nil {
		c.mu.Unlock()
		return
	}
	var effects []func()
	switch {
	case err == nil:
		// Push 受付成功と端末到達成功は別（02 §7）。到達は ready で確認する
		log.Info("voip push accepted by APNs", "latency_ms", time.Since(started).Milliseconds())
		if a.call.State == StatePushing {
			a.call.State = StateWaitingReady
			_ = c.store.SaveCall(ctx, a.call)
			c.notifyLocked(a)
		}
	case errors.Is(err, apns.ErrTokenInvalid):
		log.Warn("voip token invalid; unlinking")
		_ = c.store.ClearVoIPToken(ctx, device.ID, device.VoIPToken)
		effects = c.endLocked(ctx, a, ReasonDeviceUnreachable)
	default:
		log.Error("voip push failed", "error", err)
		effects = c.endLocked(ctx, a, ReasonDeviceUnreachable)
	}
	c.mu.Unlock()
	run(effects)
}

func (c *Controller) onTimeout(callID string) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	c.mu.Lock()
	a := c.calls[callID]
	var effects []func()
	if a != nil && a.call.State != StateConnected {
		c.log.Info("ring timeout", "call_id", callID)
		effects = c.endLocked(ctx, a, ReasonTimeout)
	}
	c.mu.Unlock()
	run(effects)
}

// --- 端末からの API

// Ready は SIP 登録を終えた端末の準備完了（03 §5.5）。冪等。
func (c *Controller) Ready(ctx context.Context, deviceID, callID, registrationID string) (Status, error) {
	c.mu.Lock()
	a := c.calls[callID]
	if a == nil {
		c.mu.Unlock()
		return c.storedStatus(ctx, deviceID, callID)
	}
	if a.call.DeviceID != deviceID {
		c.mu.Unlock()
		return Status{}, ErrNotFound
	}
	var effects []func()
	if a.call.State == StatePushing || a.call.State == StateWaitingReady {
		a.call.State = StateInviting
		a.registrationID = registrationID
		_ = c.store.SaveCall(ctx, a.call)
		c.notifyLocked(a)
		effects = append(effects, func() { go c.dialDevice(callID) })
		c.log.Info("device ready", "call_id", callID, "registration_id", secret.ShortID(registrationID))
	}
	status := statusOf(a.call)
	c.mu.Unlock()
	run(effects)
	return status, nil
}

// dialDevice は現在の Contact を照合してから端末レッグを発呼する（03 §5.5〜5.6）。
func (c *Controller) dialDevice(callID string) {
	c.mu.Lock()
	a := c.calls[callID]
	if a == nil {
		c.mu.Unlock()
		return
	}
	call, endpoint, regID := a.call, a.endpoint, a.registrationID
	c.mu.Unlock()

	ctx, cancel := ctxTimeout()
	defer cancel()
	log := c.log.With("call_id", callID, "endpoint", endpoint)

	// 古い Contact だけでは到達可能と判定しない。今回の REGISTER の接続世代が見えるまで待つ
	deadline := time.Now().Add(c.opts.ContactWait)
	if call.ExpiresAt.Before(deadline) {
		deadline = call.ExpiresAt
	}
	found := false
	for {
		contacts, err := c.ari.GetGlobal(ctx, fmt.Sprintf("PJSIP_DIAL_CONTACTS(%s)", endpoint))
		if err == nil && strings.Contains(contacts, "myvoip-reg="+regID) {
			found = true
			break
		}
		if time.Now().After(deadline) {
			log.Warn("registered contact not found", "error", err)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	c.mu.Lock()
	a = c.calls[callID]
	if a == nil {
		c.mu.Unlock()
		return
	}
	if !found {
		effects := c.endLocked(ctx, a, ReasonDeviceUnreachable)
		c.mu.Unlock()
		run(effects)
		return
	}
	channelID := "myvoip-dev-" + callID
	a.call.DeviceChannel = channelID
	c.byChannel[channelID] = callID
	_ = c.store.SaveCall(ctx, a.call)
	remaining := int(time.Until(a.call.ExpiresAt).Seconds())
	if remaining < 1 {
		remaining = 1
	}
	callerChannel := a.call.CallerChannel
	c.mu.Unlock()

	err := c.ari.CreateChannel(ctx, "PJSIP/"+endpoint, "device,"+callID, channelID)
	if err == nil {
		// 相関ヘッダーで Push と同じ呼に結び付ける。外部由来の同名ヘッダーは新しいチャネルに引き継がれない
		err = c.ari.SetVariable(ctx, channelID, "PJSIP_HEADER(add,X-MyVoIP-Call-ID)", callID)
	}
	if err == nil {
		_ = c.ari.SetVariable(ctx, channelID, "CALLERID(num)", call.CallerNumber)
		if call.CallerName != "" {
			_ = c.ari.SetVariable(ctx, channelID, "CALLERID(name)", call.CallerName)
		}
		err = c.ari.Dial(ctx, channelID, callerChannel, remaining)
	}
	if err != nil {
		log.Error("device leg dial failed", "error", err)
		c.mu.Lock()
		var effects []func()
		if a := c.calls[callID]; a != nil {
			effects = c.endLocked(ctx, a, ReasonDeviceUnreachable)
		}
		c.mu.Unlock()
		run(effects)
		return
	}
	log.Info("device leg dialing")
}

func (c *Controller) onDeviceRinging(channelID string) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.calls[c.byChannel[channelID]]
	if a == nil || a.call.DeviceChannel != channelID || a.call.State != StateInviting {
		return
	}
	a.call.State = StateRinging
	_ = c.store.SaveCall(ctx, a.call)
	c.notifyLocked(a)
}

// onDeviceAnswered は端末レッグの応答（Stasis 入り）。発信側へ最終応答してブリッジする（03 §5.8）。
func (c *Controller) onDeviceAnswered(callID, channelID string) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	c.mu.Lock()
	a := c.calls[callID]
	if a == nil || a.call.State == StateEnded || a.call.DeviceChannel != channelID {
		c.mu.Unlock()
		// 既に終了した呼へ接続しない
		_ = c.ari.Hangup(ctx, channelID, "normal")
		return
	}
	now := time.Now()
	a.call.State = StateConnected
	a.call.AnsweredAt = &now
	a.timer.Stop()
	_ = c.store.SaveCall(ctx, a.call)
	c.notifyLocked(a)
	isTest := a.call.IsTest
	callerChannel := a.call.CallerChannel
	bridgeID := "myvoip-br-" + callID
	a.bridgeID = bridgeID
	if isTest {
		// テスト着信は最長40秒で終える
		a.testTimer = time.AfterFunc(40*time.Second, func() { c.endByID(callID, ReasonCompleted) })
	}
	c.mu.Unlock()

	c.log.Info("device answered", "call_id", callID)
	if isTest {
		if err := c.ari.Play(ctx, channelID, c.opts.TestSound, testPlaybackID(callID, 1)); err != nil {
			c.log.Warn("test prompt failed", "error", err)
			c.startTestRecording(callID, channelID)
		}
		return
	}
	err := c.ari.Answer(ctx, callerChannel)
	if err == nil {
		err = c.ari.CreateBridge(ctx, bridgeID)
	}
	if err == nil {
		err = c.ari.AddToBridge(ctx, bridgeID, callerChannel, channelID)
	}
	if err != nil {
		c.log.Error("bridge failed", "call_id", callID, "error", err)
		c.endByID(callID, ReasonServerError)
	}
}

// onChannelGone は発信側・端末レッグの切断。
func (c *Controller) onChannelGone(channelID string, cause int, destroyed bool) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	c.mu.Lock()
	callID := c.byChannel[channelID]
	a := c.calls[callID]
	if a == nil {
		if destroyed {
			delete(c.byChannel, channelID)
		}
		c.mu.Unlock()
		return
	}
	var reason string
	switch channelID {
	case a.call.CallerChannel:
		if a.call.State == StateConnected {
			reason = ReasonCompleted
		} else {
			reason = ReasonCallerCancelled
		}
	case a.call.DeviceChannel:
		if !destroyed {
			// 端末レッグは応答後に Stasis へ入るため、StasisEnd は切断時だけ扱う
			if a.call.State != StateConnected {
				c.mu.Unlock()
				return
			}
			reason = ReasonCompleted
		} else if a.call.State == StateConnected {
			reason = ReasonCompleted
		} else if cause == 17 || cause == 21 {
			// 486 Busy / 603 Decline
			reason = ReasonDeclined
		} else {
			reason = ReasonDeviceUnreachable
		}
	default:
		c.mu.Unlock()
		return
	}
	effects := c.endLocked(ctx, a, reason)
	c.mu.Unlock()
	run(effects)
}

// Decline は端末からの拒否（F-14）。終端状態なら追加作用なし。
func (c *Controller) Decline(ctx context.Context, deviceID, callID, reason string) (Status, error) {
	c.mu.Lock()
	a := c.calls[callID]
	if a == nil {
		c.mu.Unlock()
		return c.storedStatus(ctx, deviceID, callID)
	}
	if a.call.DeviceID != deviceID {
		c.mu.Unlock()
		return Status{}, ErrNotFound
	}
	endReason := ReasonDeclined
	if reason == "busy" {
		endReason = ReasonBusy
	}
	effects := c.endLocked(ctx, a, endReason)
	status := statusOf(a.call)
	c.mu.Unlock()
	run(effects)
	return status, nil
}

// Status は呼の現在状態。別端末からは見えない。
func (c *Controller) Status(ctx context.Context, deviceID, callID string) (Status, error) {
	c.mu.Lock()
	if a := c.calls[callID]; a != nil {
		defer c.mu.Unlock()
		if a.call.DeviceID != deviceID {
			return Status{}, ErrNotFound
		}
		return statusOf(a.call), nil
	}
	c.mu.Unlock()
	return c.storedStatus(ctx, deviceID, callID)
}

func (c *Controller) storedStatus(ctx context.Context, deviceID, callID string) (Status, error) {
	call, err := c.store.GetCall(ctx, callID)
	if err != nil || call.DeviceID != deviceID {
		return Status{}, ErrNotFound
	}
	return statusOf(call), nil
}

// Subscribe は呼状態の通知を受ける。呼が進行中でなければ ch は nil。
func (c *Controller) Subscribe(ctx context.Context, deviceID, callID string) (Status, chan Status, func(), error) {
	c.mu.Lock()
	a := c.calls[callID]
	if a == nil {
		c.mu.Unlock()
		status, err := c.storedStatus(ctx, deviceID, callID)
		return status, nil, func() {}, err
	}
	if a.call.DeviceID != deviceID {
		c.mu.Unlock()
		return Status{}, nil, func() {}, ErrNotFound
	}
	ch := make(chan Status, 8)
	if c.subs[callID] == nil {
		c.subs[callID] = map[chan Status]struct{}{}
	}
	c.subs[callID][ch] = struct{}{}
	status := statusOf(a.call)
	c.mu.Unlock()
	cancel := func() {
		c.mu.Lock()
		delete(c.subs[callID], ch)
		if len(c.subs[callID]) == 0 {
			delete(c.subs, callID)
		}
		c.mu.Unlock()
	}
	return status, ch, cancel, nil
}

// DeviceBusy は端末が通話・着信処理中か。
func (c *Controller) DeviceBusy(deviceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deviceBusyLocked(deviceID)
}

func (c *Controller) deviceBusyLocked(deviceID string) bool {
	for _, a := range c.calls {
		if a.call.DeviceID == deviceID && a.call.State != StateEnded {
			return true
		}
	}
	return false
}

// StartTestCall は端末宛ての実在する短い着信を作る（F-02）。
func (c *Controller) StartTestCall(ctx context.Context, device store.Device) (string, error) {
	ext, err := c.store.GetExtension(ctx, device.Extension)
	if err != nil {
		return "", err
	}
	if device.VoIPToken == "" {
		return "", ErrNoPushToken
	}
	now := time.Now()
	call := store.Call{
		ID:         secret.NewID(),
		DeviceID:   device.ID,
		Extension:  device.Extension,
		CallerName: "MyVoIP テスト着信",
		State:      StatePushing,
		IsTest:     true,
		CreatedAt:  now,
		ExpiresAt:  now.Add(c.opts.RingTimeout),
	}
	c.mu.Lock()
	if c.deviceBusyLocked(device.ID) {
		c.mu.Unlock()
		return "", ErrBusy
	}
	a := &active{call: call, device: device, endpoint: ext.Endpoint}
	c.calls[call.ID] = a
	_ = c.store.SaveCall(ctx, call)
	a.timer = time.AfterFunc(time.Until(call.ExpiresAt), func() { c.onTimeout(call.ID) })
	c.mu.Unlock()
	c.log.Info("test call created", "call_id", call.ID, "device_id", device.ID)
	go c.sendPush(call.ID)
	return call.ID, nil
}

// テスト着信: ガイダンス → 4秒録音 → 録音を再生 → 終了（送受双方の音声を確認する）。
func testPlaybackID(callID string, step int) string {
	return fmt.Sprintf("myvoip-test-%s-%d", callID, step)
}
func testRecordingName(callID string) string { return "myvoip-test-" + callID }

func (c *Controller) onPlaybackFinished(playbackID string) {
	if !strings.HasPrefix(playbackID, "myvoip-test-") {
		return
	}
	rest := strings.TrimPrefix(playbackID, "myvoip-test-")
	idx := strings.LastIndex(rest, "-")
	if idx < 0 {
		return
	}
	callID, step := rest[:idx], rest[idx+1:]
	c.mu.Lock()
	a := c.calls[callID]
	var channelID string
	if a != nil {
		channelID = a.call.DeviceChannel
	}
	c.mu.Unlock()
	if a == nil {
		return
	}
	switch step {
	case "1":
		c.startTestRecording(callID, channelID)
	case "2":
		c.endByID(callID, ReasonCompleted)
	}
}

func (c *Controller) startTestRecording(callID, channelID string) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	if err := c.ari.Record(ctx, channelID, testRecordingName(callID), 4); err != nil {
		c.log.Warn("test recording failed", "call_id", callID, "error", err)
		c.endByID(callID, ReasonCompleted)
	}
}

func (c *Controller) onRecordingFinished(name string) {
	if !strings.HasPrefix(name, "myvoip-test-") {
		return
	}
	callID := strings.TrimPrefix(name, "myvoip-test-")
	c.mu.Lock()
	a := c.calls[callID]
	var channelID string
	if a != nil {
		channelID = a.call.DeviceChannel
	}
	c.mu.Unlock()
	if a == nil {
		return
	}
	ctx, cancel := ctxTimeout()
	defer cancel()
	if err := c.ari.Play(ctx, channelID, "recording:"+name, testPlaybackID(callID, 2)); err != nil {
		c.endByID(callID, ReasonCompleted)
	}
}

func (c *Controller) endByID(callID, reason string) {
	ctx, cancel := ctxTimeout()
	defer cancel()
	c.mu.Lock()
	var effects []func()
	if a := c.calls[callID]; a != nil {
		effects = c.endLocked(ctx, a, reason)
	}
	c.mu.Unlock()
	run(effects)
}

// endLocked は呼を終端状態にし、後片付けの副作用を返す。終了済みなら何もしない（03 §5.9）。
func (c *Controller) endLocked(ctx context.Context, a *active, reason string) []func() {
	if a.call.State == StateEnded {
		return nil
	}
	now := time.Now()
	wasConnected := a.call.State == StateConnected
	a.call.State = StateEnded
	a.call.EndReason = reason
	a.call.EndedAt = &now
	if a.timer != nil {
		a.timer.Stop()
	}
	if a.testTimer != nil {
		a.testTimer.Stop()
	}
	_ = c.store.SaveCall(ctx, a.call)
	c.addHistoryLocked(ctx, a.call)
	c.notifyLocked(a)
	delete(c.calls, a.call.ID)
	delete(c.byChannel, a.call.CallerChannel)
	delete(c.byChannel, a.call.DeviceChannel)
	c.log.Info("call ended", "call_id", a.call.ID, "reason", reason, "connected", wasConnected)

	call, device, bridgeID := a.call, a.device, a.bridgeID
	var effects []func()
	effects = append(effects, func() {
		ctx, cancel := ctxTimeout()
		defer cancel()
		if call.CallerChannel != "" && reason != ReasonCallerCancelled {
			_ = c.ari.Hangup(ctx, call.CallerChannel, callerHangupReason(reason))
		}
		if call.DeviceChannel != "" {
			_ = c.ari.Hangup(ctx, call.DeviceChannel, "normal")
		}
		if bridgeID != "" {
			_ = c.ari.DestroyBridge(ctx, bridgeID)
		}
		if call.IsTest {
			_ = c.ari.DeleteStoredRecording(ctx, testRecordingName(call.ID))
		}
	})
	// 不在着信は一般通知で知らせる（F-17）。取消用の VoIP Push は送らない（F-44）
	if !call.IsTest && !wasConnected && (reason == ReasonCallerCancelled || reason == ReasonTimeout) && device.AlertToken != "" {
		effects = append(effects, func() {
			ctx, cancel := ctxTimeout()
			defer cancel()
			from := call.CallerName
			if from == "" {
				from = call.CallerNumber
			}
			if from == "" {
				from = "番号非通知"
			}
			if err := c.push.SendAlert(ctx, device.AlertToken, device.PushEnvironment, "不在着信", from); err != nil {
				c.log.Warn("missed call alert failed", "call_id", call.ID, "error", err)
			}
		})
	}
	return effects
}

func callerHangupReason(reason string) string {
	switch reason {
	case ReasonTimeout, ReasonDeviceUnreachable:
		return "no_answer"
	case ReasonDeclined, ReasonBusy, ReasonPaused:
		return "busy"
	case ReasonServerError:
		return "congestion"
	default:
		return "normal"
	}
}

func (c *Controller) notifyLocked(a *active) {
	status := statusOf(a.call)
	for ch := range c.subs[a.call.ID] {
		select {
		case ch <- status:
		default:
		}
	}
}

// addHistoryLocked はアプリ用履歴を記録する（F-17: 拒否・話中・接続失敗を区別）。
func (c *Controller) addHistoryLocked(ctx context.Context, call store.Call) {
	if call.IsTest {
		return
	}
	result, detail := historyResult(call)
	item := store.HistoryItem{
		ID:           "h-" + call.ID,
		Extension:    call.Extension,
		CallID:       call.ID,
		Direction:    "incoming",
		Result:       result,
		RemoteNumber: call.CallerNumber,
		RemoteName:   call.CallerName,
		StartedAt:    call.CreatedAt,
		Detail:       detail,
	}
	if call.AnsweredAt != nil && call.EndedAt != nil {
		d := int(call.EndedAt.Sub(*call.AnsweredAt).Seconds())
		item.DurationSeconds = &d
	}
	if err := c.store.AddHistory(ctx, item); err != nil {
		c.log.Error("history write failed", "call_id", call.ID, "error", err)
	}
}

func historyResult(call store.Call) (string, string) {
	if call.AnsweredAt != nil {
		return "answered", "通話終了"
	}
	switch call.EndReason {
	case ReasonCallerCancelled:
		return "missed", "発信者が切断"
	case ReasonTimeout:
		return "missed", "応答なし"
	case ReasonPaused:
		return "missed", "着信を停止中"
	case ReasonDeclined:
		return "declined", "拒否"
	case ReasonBusy:
		return "busy", "通話中"
	case ReasonDeviceUnreachable:
		return "failed", "端末に接続できませんでした"
	default:
		return "failed", "サーバーで終了"
	}
}

func statusOf(call store.Call) Status {
	s := Status{
		ID:           call.ID,
		State:        call.State,
		ExpiresAt:    float64(call.ExpiresAt.UnixMilli()) / 1000,
		CallerNumber: call.CallerNumber,
		CallerName:   call.CallerName,
	}
	if call.EndReason != "" {
		r := call.EndReason
		s.EndReason = &r
	}
	return s
}

// Reconcile は再起動前に残った呼を整理する（R-05: 孤立呼を終了）。
func (c *Controller) Reconcile(ctx context.Context) {
	calls, err := c.store.ListUnfinishedCalls(ctx)
	if err != nil {
		c.log.Error("reconcile failed", "error", err)
		return
	}
	for _, call := range calls {
		for _, ch := range []string{call.CallerChannel, call.DeviceChannel} {
			if ch != "" {
				hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				_ = c.ari.Hangup(hctx, ch, "congestion")
				cancel()
			}
		}
		now := time.Now()
		call.State = StateEnded
		call.EndReason = ReasonServerError
		call.EndedAt = &now
		_ = c.store.SaveCall(ctx, call)
		c.mu.Lock()
		c.addHistoryLocked(ctx, call)
		c.mu.Unlock()
		c.log.Warn("orphaned call ended after restart", "call_id", call.ID)
	}
}
