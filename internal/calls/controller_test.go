package calls

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nemnet-lab/MyVoIP-Server/internal/apns"
	"github.com/nemnet-lab/MyVoIP-Server/internal/ari"
	"github.com/nemnet-lab/MyVoIP-Server/internal/store"
)

// fakeARI は ARI 呼び出しを記録し、Contact を返す。
type fakeARI struct {
	mu         sync.Mutex
	ops        []string
	contacts   string // 発信側チャネル上で評価した PJSIP_DIAL_CONTACTS の値
	state      string // エンドポイントの状態
	failDial   bool
	failRecord bool
	// varConflicts は SetVariable が「Channel not in Stasis application」（409）を返す残り回数
	varConflicts int
}

func (f *fakeARI) record(op string) {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	f.mu.Unlock()
}

func (f *fakeARI) has(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.ops {
		if strings.HasPrefix(op, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeARI) Ring(_ context.Context, ch string) error { f.record("ring " + ch); return nil }
func (f *fakeARI) Answer(_ context.Context, ch string) error {
	f.record("answer " + ch)
	return nil
}
func (f *fakeARI) Hangup(_ context.Context, ch, reason string) error {
	f.record("hangup " + ch + " " + reason)
	return nil
}
func (f *fakeARI) CreateChannel(_ context.Context, endpoint, args, id string) error {
	f.record("create " + endpoint + " " + args + " " + id)
	return nil
}
func (f *fakeARI) SetVariable(_ context.Context, ch, name, value string) error {
	f.mu.Lock()
	if f.varConflicts > 0 {
		f.varConflicts--
		f.mu.Unlock()
		return fmt.Errorf(`ari POST /channels/%s/variable: 409 {"message":"Channel not in Stasis application"}`, ch)
	}
	f.mu.Unlock()
	f.record("var " + ch + " " + name + "=" + value)
	return nil
}
func (f *fakeARI) Dial(_ context.Context, ch, caller string, timeout int) error {
	f.record(fmt.Sprintf("dial %s caller=%s", ch, caller))
	if f.failDial {
		return fmt.Errorf("dial failed")
	}
	return nil
}
func (f *fakeARI) CreateBridge(_ context.Context, id string) error {
	f.record("bridge " + id)
	return nil
}
func (f *fakeARI) AddToBridge(_ context.Context, id string, chs ...string) error {
	f.record("add " + id + " " + strings.Join(chs, ","))
	return nil
}
func (f *fakeARI) DestroyBridge(_ context.Context, id string) error {
	f.record("destroy " + id)
	return nil
}
func (f *fakeARI) GetChannelVar(_ context.Context, ch, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch == "" {
		return "", fmt.Errorf("no channel")
	}
	return f.contacts, nil
}
func (f *fakeARI) EndpointState(_ context.Context, tech, resource string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == "" {
		return "offline", nil
	}
	return f.state, nil
}
func (f *fakeARI) RemoveFromBridge(_ context.Context, id string, chs ...string) error {
	f.record("remove " + id + " " + strings.Join(chs, ","))
	return nil
}
func (f *fakeARI) Play(_ context.Context, ch, media, id string) error {
	f.record("play " + ch + " " + media + " " + id)
	return nil
}
func (f *fakeARI) Record(_ context.Context, ch, name string, max int) error {
	f.record("record " + ch + " " + name)
	if f.failRecord {
		return fmt.Errorf("Unrecognized recording error: No such file or directory")
	}
	return nil
}
func (f *fakeARI) DeleteStoredRecording(_ context.Context, name string) error {
	f.record("delrec " + name)
	return nil
}

type fakePusher struct {
	mu     sync.Mutex
	voip   []apns.VoIPPayload
	alerts []string
	err    error
	sent   chan struct{}
}

func (p *fakePusher) SendVoIP(_ context.Context, token, env string, payload apns.VoIPPayload) error {
	p.mu.Lock()
	p.voip = append(p.voip, payload)
	err := p.err
	p.mu.Unlock()
	if p.sent != nil {
		p.sent <- struct{}{}
	}
	return err
}

func (p *fakePusher) SendAlert(_ context.Context, token, env, title, body string) error {
	p.mu.Lock()
	p.alerts = append(p.alerts, title+":"+body)
	p.mu.Unlock()
	return nil
}

type fixture struct {
	t      *testing.T
	store  *store.Memory
	ari    *fakeARI
	push   *fakePusher
	ctrl   *Controller
	device store.Device
}

func newFixture(t *testing.T, ring time.Duration) *fixture {
	t.Helper()
	st := store.NewMemory()
	ctx := context.Background()
	_ = st.UpsertExtension(ctx, store.Extension{Number: "201", SIPUsername: "201", Endpoint: "201", RingTimeoutSeconds: 30})
	d := store.Device{ID: "d-1", Extension: "201", PushEnvironment: "sandbox"}
	_ = st.CreateDevice(ctx, d, time.Now())
	_ = st.UpdateDevicePush(ctx, "d-1", "aabbcc", "ddeeff", "sandbox")
	d, _ = st.GetDevice(ctx, "d-1")
	f := &fixture{t: t, store: st, ari: &fakeARI{}, push: &fakePusher{sent: make(chan struct{}, 4)}, device: d}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.ctrl = New(st, f.ari, f.push, logger, Options{RingTimeout: ring, ContactWait: 300 * time.Millisecond, TestPromptLimit: 300 * time.Millisecond, DeviceStasisWait: 50 * time.Millisecond})
	return f
}

// incoming は MikoPBX から着信が Stasis に入ったことを模擬し、Push 送信まで待つ。
func (f *fixture) incoming(callerChannel string) string {
	f.t.Helper()
	f.ctrl.HandleEvent(ari.Event{Type: "StasisStart", Args: []string{"201"}, Channel: &ari.Channel{ID: callerChannel, Caller: ari.Caller{Number: "0312345678", Name: "代表"}}})
	select {
	case <-f.push.sent:
	case <-time.After(2 * time.Second):
		f.t.Fatal("push not sent")
	}
	f.push.mu.Lock()
	defer f.push.mu.Unlock()
	return f.push.voip[len(f.push.voip)-1].CallID
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func (f *fixture) status(id string) Status {
	s, err := f.ctrl.Status(context.Background(), "d-1", id)
	if err != nil {
		f.t.Fatalf("status: %v", err)
	}
	return s
}

func TestIncomingAnsweredAndCompleted(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-1")
	if !f.ari.has("ring caller-1") {
		t.Fatal("caller should receive ringing")
	}
	payload := f.push.voip[0]
	if payload.CallerNumber != "0312345678" || payload.DeviceID != "d-1" || payload.ExpiresAt <= payload.SentAt {
		t.Fatalf("bad payload %+v", payload)
	}
	waitFor(t, func() bool { return f.status(id).State == StateWaitingReady })

	// 古い登録が残っていても、今回の接続世代の Contact だけを呼ぶ
	f.ari.contacts = "PJSIP/201/sip:201@10.0.0.5:40000;myvoip-reg=old999&PJSIP/201/sip:201@10.0.0.5:5061;transport=tls;myvoip-reg=abc123"
	st, err := f.ctrl.Ready(context.Background(), "d-1", id, "abc123")
	if err != nil || st.State != StateInviting {
		t.Fatalf("ready: %v %+v", err, st)
	}
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh + " caller=caller-1") })
	if !f.ari.has("var " + devCh + " PJSIP_HEADER(add,X-MyVoIP-Call-ID)=" + id) {
		t.Fatal("correlation header not set")
	}
	if !f.ari.has("create PJSIP/201/sip:201@10.0.0.5:5061;transport=tls;myvoip-reg=abc123 device," + id) {
		t.Fatalf("device leg must target the current contact, ops=%v", f.ari.ops)
	}

	// 重複 ready は同じ結果で、二重発呼しない（R-03）
	st2, _ := f.ctrl.Ready(context.Background(), "d-1", id, "abc123")
	if st2.State != StateInviting {
		t.Fatalf("duplicate ready changed state: %s", st2.State)
	}

	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: devCh, State: "Up"}})
	if f.status(id).State != StateConnected {
		t.Fatal("should be connected")
	}
	if !f.ari.has("answer caller-1") || !f.ari.has("add myvoip-br-"+id+" caller-1,"+devCh) {
		t.Fatal("caller not answered/bridged")
	}

	f.ctrl.HandleEvent(ari.Event{Type: "ChannelDestroyed", Channel: &ari.Channel{ID: "caller-1"}, Cause: 16})
	st = f.status(id)
	if st.State != StateEnded || *st.EndReason != ReasonCompleted {
		t.Fatalf("expected completed, got %+v", st)
	}
	if !f.ari.has("hangup "+devCh) || !f.ari.has("destroy myvoip-br-"+id) {
		t.Fatal("cleanup missing")
	}
	items, _ := f.store.ListHistory(context.Background(), "201", 0, 10)
	if len(items) != 1 || items[0].Result != "answered" || items[0].DurationSeconds == nil {
		t.Fatalf("history %+v", items)
	}
}

func TestCallerCancelBeforeReadyLeavesNoGhostCall(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-2")
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelDestroyed", Channel: &ari.Channel{ID: "caller-2"}})
	st := f.status(id)
	if st.State != StateEnded || *st.EndReason != ReasonCallerCancelled {
		t.Fatalf("got %+v", st)
	}
	// 終了後の ready は ended を返し、発呼しない
	st, err := f.ctrl.Ready(context.Background(), "d-1", id, "x")
	if err != nil || st.State != StateEnded {
		t.Fatalf("late ready: %v %+v", err, st)
	}
	if f.ari.has("create ") {
		t.Fatal("must not dial ended call")
	}
	if f.ari.has("hangup caller-2") {
		t.Fatal("caller already gone; no hangup needed")
	}
	// 不在着信の一般通知（F-17）
	waitFor(t, func() bool { f.push.mu.Lock(); defer f.push.mu.Unlock(); return len(f.push.alerts) == 1 })
	items, _ := f.store.ListHistory(context.Background(), "201", 0, 10)
	if items[0].Result != "missed" {
		t.Fatalf("history %+v", items)
	}
}

func TestLateDeviceAnswerAfterEndIsHungUp(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-3")
	f.ari.contacts = "PJSIP/201/sip:201@10.0.0.5:5060;myvoip-reg=r1"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "r1")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh) })
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelDestroyed", Channel: &ari.Channel{ID: "caller-3"}})
	// 取消直後に端末が応答しても接続しない
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: devCh, State: "Up"}})
	if f.ari.has("answer caller-3") {
		t.Fatal("must not answer cancelled caller")
	}
	if f.status(id).State != StateEnded {
		t.Fatal("state must stay ended")
	}
}

func TestRingTimeout(t *testing.T) {
	f := newFixture(t, 15*time.Second)
	// 内線の呼出時間を短くして期限切れを確かめる
	f.ctrl.opts.RingTimeout = 200 * time.Millisecond
	_ = f.store.UpsertExtension(context.Background(), store.Extension{Number: "201", SIPUsername: "201", Endpoint: "201", RingTimeoutSeconds: 0})
	id := f.incoming("caller-4")
	waitFor(t, func() bool { return f.status(id).State == StateEnded })
	if *f.status(id).EndReason != ReasonTimeout || !f.ari.has("hangup caller-4 no_answer") {
		t.Fatalf("expected timeout hangup, ops=%v", f.ari.ops)
	}
}

// /channels/create の直後はまだ Stasis に入っておらず、変数の設定が 409 になることがある。待ってやり直す
func TestDeviceLegWaitsForStasisBeforeSettingVariables(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	f.ctrl.opts.DeviceStasisWait = 2 * time.Second
	f.ari.varConflicts = 2
	id := f.incoming("caller-11")
	f.ari.contacts = "PJSIP/201/sip:201@10.0.0.5:5060;myvoip-reg=r11"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "r11")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("create ") })
	started := time.Now()
	// 端末レッグの StasisStart で待ちが解ける（DeviceStasisWait の 2 秒を待たない）
	f.ctrl.HandleEvent(ari.Event{Type: "StasisStart", Args: []string{"device", id}, Channel: &ari.Channel{ID: devCh}})
	waitFor(t, func() bool { return f.ari.has("dial " + devCh + " caller=caller-11") })
	if time.Since(started) > time.Second {
		t.Fatal("dial should not wait for the full DeviceStasisWait after StasisStart")
	}
	if !f.ari.has("var " + devCh + " PJSIP_HEADER(add,X-MyVoIP-Call-ID)=" + id) {
		t.Fatal("correlation header must be set after retrying 409")
	}
	if f.status(id).State == StateEnded {
		t.Fatal("call must not end on a transient 409")
	}
}

// 端末が鳴っていた（180）のに応答しなかった場合は、到達不能ではなく呼出時間切れ
func TestRingingDeviceNoAnswerIsTimeout(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-12")
	f.ari.contacts = "PJSIP/201/sip:201@10.0.0.5:5060;myvoip-reg=r12"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "r12")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh) })
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: devCh, State: "Ringing"}})
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelDestroyed", Channel: &ari.Channel{ID: devCh}, Cause: 16})
	st := f.status(id)
	if st.State != StateEnded || *st.EndReason != ReasonTimeout {
		t.Fatalf("expected timeout, got %+v", st)
	}
	if !f.ari.has("hangup caller-12 no_answer") {
		t.Fatalf("caller should get no_answer, ops=%v", f.ari.ops)
	}
}

func TestContactMustMatchRegistration(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-5")
	// 古い Contact しかない（接続世代の前方一致にも反応しない）
	f.ari.contacts = "PJSIP/201/sip:201@10.0.0.5:5061;myvoip-reg=old"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "new")
	waitFor(t, func() bool { return f.status(id).State == StateEnded })
	if *f.status(id).EndReason != ReasonDeviceUnreachable || f.ari.has("create ") {
		t.Fatal("must not dial stale contact")
	}
}

func TestCurrentContact(t *testing.T) {
	contacts := "PJSIP/9002/sip:9002@100.101.1.2:51000;myvoip-reg=aaa&PJSIP/9002/sip:9002@100.101.1.2:52000;myvoip-reg=bbb"
	if got := currentContact(contacts, "bbb"); got != "PJSIP/9002/sip:9002@100.101.1.2:52000;myvoip-reg=bbb" {
		t.Fatalf("got %q", got)
	}
	if currentContact(contacts, "ccc") != "" || currentContact(contacts, "") != "" || currentContact("", "aaa") != "" {
		t.Fatal("unexpected match")
	}
}

func TestRemovedContactIsForgotten(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	uri := "sip:201@100.64.0.30:50000;myvoip-reg=r9"
	ev := func(status string) ari.Event {
		return ari.Event{Type: "ContactStatusChange", Endpoint: &ari.EndpointInfo{Resource: "201"},
			ContactInfo: &ari.ContactInfo{URI: uri, ContactStatus: status, AOR: "201"}}
	}
	f.ctrl.HandleEvent(ev("Created"))
	if f.ctrl.knownContact("201", "r9") == "" {
		t.Fatal("created contact should be known")
	}
	f.ctrl.HandleEvent(ev("Removed"))
	if f.ctrl.knownContact("201", "r9") != "" {
		t.Fatal("removed contact should be forgotten")
	}
}

func TestFallsBackToEndpointWhenOnline(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id, _ := f.ctrl.StartTestCall(context.Background(), f.device)
	<-f.push.sent
	f.ari.state = "online"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "unknown")
	waitFor(t, func() bool { return f.ari.has("create PJSIP/201 device," + id) })
}

func TestTestCallFailsWhenEndpointOffline(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id, _ := f.ctrl.StartTestCall(context.Background(), f.device)
	<-f.push.sent
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "unknown")
	waitFor(t, func() bool { return f.status(id).State == StateEnded })
	if *f.status(id).EndReason != ReasonDeviceUnreachable || f.ari.has("create ") {
		t.Fatal("offline endpoint must not be dialed")
	}
}

// /channels/create の端末レッグは作成時に Stasis へ入る。これを応答とみなしてはいけない
func TestDeviceStasisStartIsNotAnswer(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-20")
	f.ari.contacts = "PJSIP/201/sip:201@10.0.0.5:5060;myvoip-reg=s1"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "s1")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh) })
	f.ctrl.HandleEvent(ari.Event{Type: "StasisStart", Args: []string{"device", id}, Channel: &ari.Channel{ID: devCh, State: "Down"}})
	if f.status(id).State == StateConnected || f.ari.has("answer caller-20") {
		t.Fatal("StasisStart of the device leg must not answer the caller")
	}
	// Dial の ANSWER でも応答として扱い、ChannelStateChange と重なっても二重にブリッジしない
	f.ctrl.HandleEvent(ari.Event{Type: "Dial", DialStatus: "ANSWER", Peer: &ari.Channel{ID: devCh}})
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: devCh, State: "Up"}})
	if f.status(id).State != StateConnected {
		t.Fatal("should be connected after answer")
	}
	f.ari.mu.Lock()
	bridges := 0
	for _, op := range f.ari.ops {
		if strings.HasPrefix(op, "bridge ") {
			bridges++
		}
	}
	f.ari.mu.Unlock()
	if bridges != 1 {
		t.Fatalf("bridge created %d times", bridges)
	}
	// 発信側の Up（こちらが応答させたもの）は端末の応答として扱わない
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: "caller-20", State: "Up"}})
}

func TestTestCallRecordingFailureEndsGracefully(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	f.ari.failRecord = true
	id, _ := f.ctrl.StartTestCall(context.Background(), f.device)
	<-f.push.sent
	f.ctrl.HandleEvent(ari.Event{Type: "ContactStatusChange", Endpoint: &ari.EndpointInfo{Resource: "201"},
		ContactInfo: &ari.ContactInfo{URI: "sip:201@100.64.0.30:1;myvoip-reg=x1", ContactStatus: "Reachable"}})
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "x1")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh) })
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: devCh, State: "Up"}})
	f.ctrl.HandleEvent(ari.Event{Type: "PlaybackFinished", Playback: &ari.Playback{ID: testPlaybackID(id, 1)}})
	// すぐには切らない
	if f.status(id).State == StateEnded {
		t.Fatal("recording failure must not end the call immediately")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && f.status(id).State != StateEnded {
		time.Sleep(50 * time.Millisecond)
	}
	if st := f.status(id); st.State != StateEnded || *st.EndReason != ReasonCompleted {
		t.Fatalf("expected completed, got %+v", st)
	}
}

// ガイダンスの終了通知が来なくても（音声ファイルがない場合など）録音へ進む
func TestTestCallProceedsWhenPromptNeverFinishes(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id, _ := f.ctrl.StartTestCall(context.Background(), f.device)
	<-f.push.sent
	f.ctrl.HandleEvent(ari.Event{Type: "ContactStatusChange", Endpoint: &ari.EndpointInfo{Resource: "201"},
		ContactInfo: &ari.ContactInfo{URI: "sip:201@100.64.0.30:1;myvoip-reg=p1", ContactStatus: "Reachable"}})
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "p1")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh) })
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: devCh, State: "Up"}})
	waitFor(t, func() bool { return f.ari.has("record " + devCh) })
	// 遅れて届いた終了通知で二重に録音しない
	f.ctrl.HandleEvent(ari.Event{Type: "PlaybackFinished", Playback: &ari.Playback{ID: testPlaybackID(id, 1), State: "failed"}})
	f.ari.mu.Lock()
	records := 0
	for _, op := range f.ari.ops {
		if strings.HasPrefix(op, "record ") {
			records++
		}
	}
	f.ari.mu.Unlock()
	if records != 1 {
		t.Fatalf("recorded %d times", records)
	}
}

func TestTestCallRecordingFailedEventEndsGracefully(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id, _ := f.ctrl.StartTestCall(context.Background(), f.device)
	<-f.push.sent
	f.ctrl.HandleEvent(ari.Event{Type: "ContactStatusChange", Endpoint: &ari.EndpointInfo{Resource: "201"},
		ContactInfo: &ari.ContactInfo{URI: "sip:201@100.64.0.30:1;myvoip-reg=q1", ContactStatus: "Reachable"}})
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "q1")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh) })
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: devCh, State: "Up"}})
	f.ctrl.HandleEvent(ari.Event{Type: "PlaybackFinished", Playback: &ari.Playback{ID: testPlaybackID(id, 1), State: "done"}})
	f.ctrl.HandleEvent(ari.Event{Type: "RecordingFailed", Recording: &ari.Recording{Name: testRecordingName(id), State: "failed", Cause: "test"}})
	if f.status(id).State == StateEnded {
		t.Fatal("must not end immediately")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && f.status(id).State != StateEnded {
		time.Sleep(50 * time.Millisecond)
	}
	if st := f.status(id); st.State != StateEnded || *st.EndReason != ReasonCompleted {
		t.Fatalf("expected completed, got %+v", st)
	}
}

// 同じイベントが複数の購読から届いても、呼を二重に作らない
func TestDuplicateEventsAreIgnored(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	ev := ari.Event{Type: "StasisStart", Timestamp: "2026-10-11T20:55:08.110+0900", Args: []string{"201"},
		Channel: &ari.Channel{ID: "caller-dup", Caller: ari.Caller{Number: "1001"}}}
	f.ctrl.HandleEvent(ev)
	<-f.push.sent
	f.ctrl.HandleEvent(ev) // 同じ内容
	// 時刻が違っても、同じ発信側チャネルの StasisStart は受け付けない
	ev.Timestamp = "2026-10-11T20:55:55.081+0900"
	f.ctrl.HandleEvent(ev)
	time.Sleep(200 * time.Millisecond)
	f.push.mu.Lock()
	pushes := len(f.push.voip)
	f.push.mu.Unlock()
	if pushes != 1 {
		t.Fatalf("pushed %d times", pushes)
	}
}

func TestDeclineAndBusy(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-6")
	st, err := f.ctrl.Decline(context.Background(), "d-1", id, "declined")
	if err != nil || *st.EndReason != ReasonDeclined || !f.ari.has("hangup caller-6 busy") {
		t.Fatalf("decline: %v %+v", err, st)
	}
	// 終端状態への拒否は追加作用なし
	st, _ = f.ctrl.Decline(context.Background(), "d-1", id, "busy")
	if *st.EndReason != ReasonDeclined {
		t.Fatal("ended state must not change")
	}
}

func TestBusyDeviceRejectsSecondCall(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	f.incoming("caller-7")
	f.ctrl.HandleEvent(ari.Event{Type: "StasisStart", Args: []string{"201"}, Channel: &ari.Channel{ID: "caller-8"}})
	if !f.ari.has("hangup caller-8 busy") {
		t.Fatal("second call should be busy (F-16)")
	}
	if len(f.push.voip) != 1 {
		t.Fatal("no push for busy call")
	}
}

func TestPausedDeviceGetsNoPush(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	_ = f.store.SetDeviceEnabled(context.Background(), "d-1", false)
	f.ctrl.HandleEvent(ari.Event{Type: "StasisStart", Args: []string{"201"}, Channel: &ari.Channel{ID: "caller-9"}})
	if !f.ari.has("hangup caller-9 busy") || len(f.push.voip) != 0 {
		t.Fatal("paused device should not be pushed")
	}
}

func TestInvalidTokenEndsCallAndUnlinks(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	f.push.err = apns.ErrTokenInvalid
	id := f.incoming("caller-10")
	waitFor(t, func() bool { return f.status(id).State == StateEnded })
	d, _ := f.store.GetDevice(context.Background(), "d-1")
	if d.VoIPToken != "" {
		t.Fatal("invalid token should be cleared")
	}
}

func TestOtherDeviceCannotSeeCall(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-11")
	if _, err := f.ctrl.Status(context.Background(), "d-other", id); err != ErrNotFound {
		t.Fatal("other device must not see the call")
	}
	if _, err := f.ctrl.Ready(context.Background(), "d-other", id, "x"); err != ErrNotFound {
		t.Fatal("other device must not ready the call")
	}
}

func TestTestCallPlaysRecordsAndEnds(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id, err := f.ctrl.StartTestCall(context.Background(), f.device)
	if err != nil {
		t.Fatal(err)
	}
	<-f.push.sent
	// テスト着信には発信側チャネルがないので、ContactStatusChange で知った登録先を使う
	f.ctrl.HandleEvent(ari.Event{Type: "ContactStatusChange",
		Endpoint:    &ari.EndpointInfo{Technology: "PJSIP", Resource: "201", State: "online"},
		ContactInfo: &ari.ContactInfo{URI: "sip:201@100.64.0.30:54482;myvoip-reg=t1", ContactStatus: "Reachable", AOR: "201"}})
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "t1")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh + " caller=") })
	if !f.ari.has("create PJSIP/201/sip:201@100.64.0.30:54482;myvoip-reg=t1 device," + id) {
		t.Fatalf("test call must target the contact from the event, ops=%v", f.ari.ops)
	}
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelStateChange", Channel: &ari.Channel{ID: devCh, State: "Up"}})
	// 保留用ブリッジから抜けさせてから、チャネルに対して再生する
	tmp := "myvoip-tmp-" + id
	if !f.ari.has("add "+tmp+" "+devCh) || !f.ari.has("remove "+tmp+" "+devCh) || !f.ari.has("play "+devCh+" sound:vm-intro") {
		t.Fatalf("test leg must leave the dial bridge and play on the channel, ops=%v", f.ari.ops)
	}
	f.ctrl.HandleEvent(ari.Event{Type: "PlaybackFinished", Playback: &ari.Playback{ID: testPlaybackID(id, 1)}})
	if !f.ari.has("record " + devCh) {
		t.Fatal("not recording the channel")
	}
	f.ctrl.HandleEvent(ari.Event{Type: "RecordingFinished", Recording: &ari.Recording{Name: testRecordingName(id)}})
	if !f.ari.has("play " + devCh + " recording:") {
		t.Fatal("recording not played back")
	}
	f.ctrl.HandleEvent(ari.Event{Type: "PlaybackFinished", Playback: &ari.Playback{ID: testPlaybackID(id, 2)}})
	waitFor(t, func() bool { return f.status(id).State == StateEnded })
	if !f.ari.has("delrec " + testRecordingName(id)) {
		t.Fatal("recording not deleted")
	}
	items, _ := f.store.ListHistory(context.Background(), "201", 0, 10)
	if len(items) != 0 {
		t.Fatal("test call must not appear in history")
	}
}

func TestReconcileEndsOrphans(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	_ = f.store.SaveCall(context.Background(), store.Call{ID: "orphan", DeviceID: "d-1", Extension: "201", State: StateInviting, CallerChannel: "c-x", CreatedAt: time.Now(), ExpiresAt: time.Now()})
	f.ctrl.Reconcile(context.Background())
	c, _ := f.store.GetCall(context.Background(), "orphan")
	if c.State != StateEnded || !f.ari.has("hangup c-x") {
		t.Fatal("orphan not cleaned")
	}
}
