package copilotsdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/chattest"
	"github.com/goobers/goobers/internal/invoke"
)

type fakeNative struct {
	mu      sync.Mutex
	handler func(sdk.SessionEvent)
	modes   []string
	aborts  int
}

func (f *fakeNative) Send(_ context.Context, o sdk.MessageOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modes = append(f.modes, o.Mode)
	return "message", nil
}
func (f *fakeNative) Abort(context.Context) error {
	f.mu.Lock()
	f.aborts++
	f.mu.Unlock()
	f.emit(&rpc.AssistantTurnEndData{})
	f.emit(&rpc.SessionIdleData{})
	return nil
}
func (f *fakeNative) On(fn sdk.SessionEventHandler) func() {
	f.mu.Lock()
	f.handler = fn
	f.mu.Unlock()
	return func() { f.mu.Lock(); f.handler = nil; f.mu.Unlock() }
}
func (f *fakeNative) SetModel(context.Context, string, *sdk.SetModelOptions) error { return nil }
func (f *fakeNative) Disconnect() error                                            { return nil }
func (f *fakeNative) emit(d rpc.SessionEventData) {
	f.mu.Lock()
	fn := f.handler
	f.mu.Unlock()
	if fn != nil {
		fn(sdk.SessionEvent{Data: d})
	}
}
func testDriver(f *fakeNative) *Driver {
	return &Driver{open: func(context.Context) (*backend, error) {
		return &backend{create: func(context.Context, *sdk.SessionConfig) (nativeSession, string, error) { return f, "native", nil }, resume: func(context.Context, string, *sdk.ResumeSessionConfig) (nativeSession, error) { return f, nil }, close: func() error { return nil }, policy: func(sdk.PermissionRequest, sdk.PermissionInvocation) (rpc.PermissionDecision, error) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}}, nil
	}}
}
func options() chat.StartOptions {
	return chat.StartOptions{SessionID: "session", Fence: 7, Level: chat.Ask, Ceiling: chat.NewCeiling(chat.Auto, chat.Auto, chat.Auto)}
}
func next(t *testing.T, s chat.Session, kind string) chat.Event {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("stream ended before %s", kind)
			}
			if e.Type == kind {
				return e
			}
		case <-timer.C:
			t.Fatalf("no %s", kind)
		}
	}
}
func TestConformance(t *testing.T) {
	chattest.Conformance(t, func() chat.Driver { return testDriver(&fakeNative{}) })
}
func TestNativeDeliveryAndCancelIdle(t *testing.T) {
	f := &fakeNative{}
	s, err := testDriver(f).Start(context.Background(), options())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, want := range []chat.Delivery{chat.Prompt, chat.Steer, chat.Queued} {
		got, err := s.Send(context.Background(), chat.Text("hello"), chat.SendOptions{Delivery: want, CommandID: "command"})
		if got != want || err != nil {
			t.Fatal(got, err)
		}
		e := next(t, s, "command.delivered")
		if e.Data["delivery"] != want || e.CommandID != "command" {
			t.Fatal(e)
		}
	}
	f.emit(&rpc.AssistantTurnEndData{})
	if _, err := s.Send(context.Background(), chat.Text("still busy"), chat.SendOptions{Delivery: chat.Prompt}); !errors.Is(err, chat.ErrBusy) {
		t.Fatal("turn_end settled before idle")
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := next(t, s, "turn_ended"); e.Data["reason"] != "cancelled" {
		t.Fatal(e)
	}
	f.emit(&rpc.AssistantTurnStartData{})
	f.emit(&rpc.SessionIdleData{})
	if e := next(t, s, "turn_ended"); e.Data["reason"] != "end_turn" {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.modes, ",") != "immediate,immediate,enqueue" {
		t.Fatal(f.modes)
	}
}
func TestResumeMissingAndStalled(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "stalled"}[stalled], func(t *testing.T) {
			home := t.TempDir()
			if stalled {
				if err := os.MkdirAll(filepath.Join(home, "session-state", "old"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			f := &fakeNative{}
			d := testDriver(f)
			base := d.open
			closed := 0
			resumed := 0
			d.open = func(ctx context.Context) (*backend, error) {
				b, _ := base(ctx)
				b.home = home
				b.close = func() error { closed++; return nil }
				b.resume = func(ctx context.Context, _ string, _ *sdk.ResumeSessionConfig) (nativeSession, error) {
					resumed++
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return b, nil
			}
			o := options()
			o.ResumeNativeID = "old"
			o.ResumeTimeout = time.Millisecond
			s, err := d.Start(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			want := chat.ResumeNotFound
			if stalled {
				want = chat.ResumeStalled
				if closed != 1 || resumed != 1 {
					t.Fatal(closed, resumed)
				}
			} else if resumed != 0 {
				t.Fatal("loaded absent session")
			}
			if s.(chat.ResumeInfo).ResumeFallback() != want {
				t.Fatal("wrong fallback")
			}
			s.Close()
		})
	}
}
func TestFailedCustodyPreventsResumeFallback(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, "session-state", "old"), 0700)
	d := testDriver(&fakeNative{})
	base := d.open
	calls := 0
	d.open = func(ctx context.Context) (*backend, error) {
		calls++
		b, _ := base(ctx)
		b.home = home
		b.close = func() error { return invoke.ErrWorkspaceNotQuiescent }
		b.resume = func(ctx context.Context, _ string, _ *sdk.ResumeSessionConfig) (nativeSession, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return b, nil
	}
	o := options()
	o.ResumeNativeID = "old"
	o.ResumeTimeout = time.Millisecond
	_, err := d.Start(context.Background(), o)
	if !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) || calls != 1 {
		t.Fatal(err, calls)
	}
}
func TestPermissionPolicyAndClose(t *testing.T) {
	d := testDriver(&fakeNative{})
	s0, err := d.Start(context.Background(), options())
	if err != nil {
		t.Fatal(err)
	}
	s := s0.(*session)
	done := make(chan rpc.PermissionDecision, 1)
	go func() {
		decision, _ := s.permission(&rpc.PermissionRequestWrite{}, sdk.PermissionInvocation{})
		done <- decision
	}()
	e := next(t, s, "permission_request")
	if _, err := s.SetLevel(context.Background(), chat.Plan); err != nil {
		t.Fatal(err)
	}
	if err := s.RespondPermission(context.Background(), e.Data["requestId"].(string), chat.PermissionResponse{OptionID: "allow_once"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := (<-done).(*rpc.PermissionDecisionApproveOnce); ok {
		t.Fatal("stale approval widened plan level")
	}
	s.SetLevel(context.Background(), chat.Ask)
	go func() {
		decision, _ := s.permission(&rpc.PermissionRequestWrite{}, sdk.PermissionInvocation{})
		done <- decision
	}()
	next(t, s, "permission_request")
	s.Close()
	select {
	case decision := <-done:
		if _, ok := decision.(*rpc.PermissionDecisionApproveOnce); ok {
			t.Fatal("close approved pending request")
		}
	case <-time.After(time.Second):
		t.Fatal("pending handler leaked")
	}
}
func TestOuterPolicyCannotBeOverridden(t *testing.T) {
	d := testDriver(&fakeNative{})
	base := d.open
	d.open = func(ctx context.Context) (*backend, error) {
		b, _ := base(ctx)
		b.policy = func(sdk.PermissionRequest, sdk.PermissionInvocation) (rpc.PermissionDecision, error) {
			return &rpc.PermissionDecisionUserNotAvailable{}, nil
		}
		return b, nil
	}
	s0, _ := d.Start(context.Background(), options())
	defer s0.Close()
	s := s0.(*session)
	s.SetLevel(context.Background(), chat.FullAccess)
	decision, _ := s.permission(&rpc.PermissionRequestWrite{}, sdk.PermissionInvocation{})
	if _, ok := decision.(*rpc.PermissionDecisionApproveOnce); ok {
		t.Fatal("outer denial bypassed")
	}
}
func TestFixtureTranslation(t *testing.T) {
	file, err := os.Open("testdata/lifecycle.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fixture, err := chattest.ReadFixture(file)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNative{}
	s, _ := testDriver(f).Start(context.Background(), options())
	defer s.Close()
	err = fixture.Replay(func() (map[string]any, error) { return nil, errors.New("unexpected client record") }, func(e chattest.Entry) error {
		data, _ := json.Marshal(e.Msg)
		var native sdk.SessionEvent
		if err := json.Unmarshal(data, &native); err != nil {
			return err
		}
		f.emit(native.Data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	types := map[string]int{}
	for e := range s.Events() {
		types[e.Type]++
		data, _ := json.Marshal(e)
		if strings.Contains(string(data), "secret") {
			t.Fatal("native params leaked")
		}
	}
	if types["turn_ended"] != 1 || types["usage"] != 1 || types["protocol_unknown"] != 1 || types["tool_call"] != 1 {
		t.Fatal(types)
	}
}

func TestRecordedTwoTurns(t *testing.T) {
	file, err := os.Open("testdata/two-turns-recorded.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fixture, err := chattest.ReadFixture(file)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNative{}
	s, _ := testDriver(f).Start(context.Background(), options())
	defer s.Close()
	err = fixture.Replay(func() (map[string]any, error) { return nil, errors.New("unexpected client message") }, func(e chattest.Entry) error {
		data, _ := json.Marshal(e.Msg)
		var native sdk.SessionEvent
		if err := json.Unmarshal(data, &native); err != nil {
			return err
		}
		f.emit(native.Data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	turns, usage := 0, 0
	for e := range s.Events() {
		if e.Type == "turn_ended" {
			turns++
		}
		if e.Type == "usage" {
			usage++
		}
	}
	if turns != 2 || usage != 2 {
		t.Fatal(turns, usage)
	}
}
