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
	mu       sync.Mutex
	ops      []string
	contacts string
	failDial bool
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
func (f *fakeARI) GetGlobal(_ context.Context, expr string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contacts, nil
}
func (f *fakeARI) Play(_ context.Context, ch, media, id string) error {
	f.record("play " + ch + " " + media + " " + id)
	return nil
}
func (f *fakeARI) Record(_ context.Context, ch, name string, max int) error {
	f.record("record " + ch + " " + name)
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
	f.ctrl = New(st, f.ari, f.push, logger, Options{RingTimeout: ring, ContactWait: 300 * time.Millisecond})
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

	f.ari.contacts = "PJSIP/201/sip:201@10.0.0.5:5061;transport=tls;myvoip-reg=abc123"
	st, err := f.ctrl.Ready(context.Background(), "d-1", id, "abc123")
	if err != nil || st.State != StateInviting {
		t.Fatalf("ready: %v %+v", err, st)
	}
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh + " caller=caller-1") })
	if !f.ari.has("var " + devCh + " PJSIP_HEADER(add,X-MyVoIP-Call-ID)=" + id) {
		t.Fatal("correlation header not set")
	}
	if !f.ari.has("create PJSIP/201 device," + id) {
		t.Fatal("device leg not created on endpoint")
	}

	// 重複 ready は同じ結果で、二重発呼しない（R-03）
	st2, _ := f.ctrl.Ready(context.Background(), "d-1", id, "abc123")
	if st2.State != StateInviting {
		t.Fatalf("duplicate ready changed state: %s", st2.State)
	}

	f.ctrl.HandleEvent(ari.Event{Type: "StasisStart", Args: []string{"device", id}, Channel: &ari.Channel{ID: devCh}})
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
	f.ari.contacts = "...;myvoip-reg=r1"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "r1")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh) })
	f.ctrl.HandleEvent(ari.Event{Type: "ChannelDestroyed", Channel: &ari.Channel{ID: "caller-3"}})
	// 取消直後に端末が応答しても接続しない
	f.ctrl.HandleEvent(ari.Event{Type: "StasisStart", Args: []string{"device", id}, Channel: &ari.Channel{ID: devCh}})
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

func TestContactMustMatchRegistration(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	id := f.incoming("caller-5")
	// 古い Contact しかない
	f.ari.contacts = "PJSIP/201/sip:201@10.0.0.5:5061;myvoip-reg=old"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "new")
	waitFor(t, func() bool { return f.status(id).State == StateEnded })
	if *f.status(id).EndReason != ReasonDeviceUnreachable || f.ari.has("create ") {
		t.Fatal("must not dial stale contact")
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
	f.ari.contacts = ";myvoip-reg=t1"
	_, _ = f.ctrl.Ready(context.Background(), "d-1", id, "t1")
	devCh := "myvoip-dev-" + id
	waitFor(t, func() bool { return f.ari.has("dial " + devCh + " caller=") })
	f.ctrl.HandleEvent(ari.Event{Type: "StasisStart", Args: []string{"device", id}, Channel: &ari.Channel{ID: devCh}})
	if !f.ari.has("play " + devCh + " sound:hello-world") {
		t.Fatal("prompt not played")
	}
	f.ctrl.HandleEvent(ari.Event{Type: "PlaybackFinished", Playback: &ari.Playback{ID: testPlaybackID(id, 1)}})
	if !f.ari.has("record " + devCh) {
		t.Fatal("not recording")
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
