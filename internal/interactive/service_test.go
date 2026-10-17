package interactive

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testExecution struct {
	req    ExecutionRequest
	emit   func(ExecutionEvent) error
	result chan ExecutionResult
	done   chan struct{}
}
type testExecutor struct {
	started      chan *testExecution
	delayStop    chan struct{}
	recoverErr   error
	recoverCount atomic.Int32
}

func newExecutor() *testExecutor { return &testExecutor{started: make(chan *testExecution, 10)} }
func (e *testExecutor) Execute(ctx context.Context, req ExecutionRequest, emit func(ExecutionEvent) error) (ExecutionResult, error) {
	run := &testExecution{req, emit, make(chan ExecutionResult, 1), make(chan struct{})}
	e.started <- run
	defer close(run.done)
	select {
	case result := <-run.result:
		return result, nil
	case <-ctx.Done():
		if e.delayStop != nil {
			<-e.delayStop
		}
		return ExecutionResult{}, ctx.Err()
	}
}
func (e *testExecutor) Recover(context.Context, ExecutionRequest) error {
	e.recoverCount.Add(1)
	return e.recoverErr
}
func options(t *testing.T, e Executor) Options {
	t.Helper()
	return Options{StateDir: t.TempDir(), Executor: e, Principal: func(*http.Request) (string, error) { return "alice", nil }, Surfaces: []Surface{{ID: "work", Name: "Workbench", Mode: "workbench", Enabled: true, MaxChildren: 2}, {ID: "help", Name: "Helper", Mode: "helper", Enabled: true, MaxChildren: 1}}}
}
func openTest(t *testing.T, opts Options) *Service {
	t.Helper()
	service, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return service
}
func createTest(t *testing.T, s *Service, owner string) Session {
	t.Helper()
	session, err := s.Create(owner, CreateRequest{CommandID: id("create_"), SurfaceID: "work"})
	if err != nil {
		t.Fatal(err)
	}
	return session
}
func commandTest(t *testing.T, s *Service, owner, sessionID string, cmd Command) CommandReceipt {
	t.Helper()
	current, err := s.Session(owner, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExpectedSequence = current.Sequence
	cmd.ExpectedFence = current.Fence
	if cmd.CommandID == "" {
		cmd.CommandID = id("cmd_")
	}
	receipt, err := s.Command(context.Background(), owner, sessionID, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}
func waitStatus(t *testing.T, s *Service, owner, sessionID, status string) Session {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session, err := s.Session(owner, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.Status == status {
			return session
		}
		time.Sleep(time.Millisecond)
	}
	session, _ := s.Session(owner, sessionID)
	t.Fatalf("status = %s; want %s", session.Status, status)
	return Session{}
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Code != code {
		t.Fatalf("error = %v; want %s", err, code)
	}
}
func receiveRun(t *testing.T, e *testExecutor) *testExecution {
	t.Helper()
	select {
	case run := <-e.started:
		return run
	case <-time.After(3 * time.Second):
		t.Fatal("executor not started")
		return nil
	}
}

func TestOwnerIsolationCommandReplayAndCumulativeChanges(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	if _, err := s.Session("bob", session.ID); err == nil {
		t.Fatal("cross-owner session read permitted")
	}
	if _, err := s.Events("bob", session.ID, 0); err == nil {
		t.Fatal("cross-owner event read permitted")
	}
	if got := s.ListSessions("bob"); len(got) != 0 {
		t.Fatal("session inventory leaked")
	}
	cmd := Command{CommandID: "stable-command", Type: "message", Text: "Investigate", ExpectedSequence: session.Sequence, ExpectedFence: session.Fence}
	receipt, err := s.Command(context.Background(), "alice", session.ID, cmd)
	if err != nil {
		t.Fatal(err)
	}
	run := receiveRun(t, e)
	if err := run.emit(ExecutionEvent{Type: "tool.started", Data: map[string]any{"name": "read_file"}}); err != nil {
		t.Fatal(err)
	}
	replay, err := s.Command(context.Background(), "alice", session.ID, cmd)
	if err != nil || replay != receipt {
		t.Fatalf("lost response replay changed: %+v %v", replay, err)
	}
	cmd.Text = "different"
	_, err = s.Command(context.Background(), "alice", session.ID, cmd)
	requireCode(t, err, "idempotency_conflict")
	cmd.CommandID = "different-id"
	cmd.ExpectedSequence = 1
	_, err = s.Command(context.Background(), "alice", session.ID, cmd)
	requireCode(t, err, "stale_sequence")
	current, _ := s.Session("alice", session.ID)
	cmd.ExpectedSequence = current.Sequence
	cmd.ExpectedFence = 0
	_, err = s.Command(context.Background(), "alice", session.ID, cmd)
	requireCode(t, err, "stale_fence")
	run.result <- ExecutionResult{Message: "Edited and tested", WorkspacePath: "/private/workspace", Changes: []Change{{Path: "app.js", Status: "modified", Additions: 2}}, Diff: "+ fixed"}
	final := waitStatus(t, s, "alice", session.ID, "idle")
	if len(final.Changes) != 1 || final.Diff != "+ fixed" || final.Lease.Status != "released" {
		t.Fatalf("bad cumulative result: %+v", final)
	}
	events, _ := s.Events("alice", session.ID, 0)
	for i, event := range events {
		if event.Sequence != uint64(i+1) || event.Actor != "alice" {
			t.Fatal("nonmonotonic or unattributed event")
		}
	}
	if len(s.active) != 0 {
		t.Fatal("idle session retained compute")
	}
}

func TestSteeringAcknowledgementAndStopFence(t *testing.T) {
	e := newExecutor()
	e.delayStop = make(chan struct{})
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	receipt := commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "Long work"})
	run := receiveRun(t, e)
	steer := commandTest(t, s, "alice", session.ID, Command{Type: "steer", Text: "Focus tests", TurnID: receipt.TurnID})
	message := <-run.req.Steering
	current, _ := s.Session("alice", session.ID)
	if s.state.Sessions[session.ID].Commands[steer.CommandID].Receipt.Status != "accepted" {
		t.Fatal("steering falsely delivered before acknowledgement")
	}
	message.Ack(nil)
	if s.state.Sessions[session.ID].Commands[steer.CommandID].Receipt.Status != "delivered" {
		t.Fatal("delivery acknowledgement missing")
	}
	stop := commandTest(t, s, "alice", session.ID, Command{Type: "stop"})
	if stop.Status != "accepted" {
		t.Fatal("stop falsely completed")
	}
	stopping := waitStatus(t, s, "alice", session.ID, "stopping")
	if stopping.Fence <= current.Fence {
		t.Fatal("stop did not fence attempt")
	}
	err := run.emit(ExecutionEvent{Type: "assistant.delta", Data: map[string]any{"text": "late output "}})
	requireCode(t, err, "stale_fence")
	close(e.delayStop)
	waitStatus(t, s, "alice", session.ID, "stopped")
	_, err = s.Command(context.Background(), "alice", session.ID, Command{CommandID: "new", Type: "message", Text: "restart", ExpectedSequence: stopping.Sequence, ExpectedFence: stopping.Fence})
	requireCode(t, err, "stale_sequence")
}

func TestRestartDoesNotReplayAndRequiresCustodyConfirmation(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "Do work"})
	receiveRun(t, e)
	crashSnapshot, err := os.ReadFile(filepath.Join(opts.StateDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opts.StateDir, "sessions.json"), crashSnapshot, 0600); err != nil {
		t.Fatal(err)
	}
	e2 := newExecutor()
	e2.recoverErr = errors.New("old process custody unverified")
	opts.Executor = e2
	s2 := openTest(t, opts)
	interrupted := waitStatus(t, s2, "alice", session.ID, "recovery_required")
	if interrupted.Fence <= session.Fence {
		t.Fatal("restart did not advance fence")
	}
	select {
	case <-e2.started:
		t.Fatal("restart replayed execution")
	default:
	}
	_, err = s2.Command(context.Background(), "alice", session.ID, Command{CommandID: "resume-fail", Type: "resume", ExpectedSequence: interrupted.Sequence, ExpectedFence: interrupted.Fence})
	requireCode(t, err, "recovery_required")
	e2.recoverErr = nil
	commandTest(t, s2, "alice", session.ID, Command{Type: "resume"})
	run := receiveRun(t, e2)
	if run.req.Attempt != 2 {
		t.Fatalf("attempt = %d", run.req.Attempt)
	}
	run.result <- ExecutionResult{Message: "recovered"}
	waitStatus(t, s2, "alice", session.ID, "idle")
}

func TestLeaseExpiryFencesOldAttemptAndSafeTakeover(t *testing.T) {
	e := newExecutor()
	e.delayStop = make(chan struct{})
	opts := options(t, e)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	opts.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	opts.LeaseDuration = 30 * time.Second
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "long"})
	run := receiveRun(t, e)
	if err := run.req.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(31 * time.Second))
	s.SweepLeases()
	expired := waitStatus(t, s, "alice", session.ID, "recovery_required")
	requireCode(t, run.req.Heartbeat(), "stale_lease")
	_, err := s.Command(context.Background(), "alice", session.ID, Command{CommandID: "premature", Type: "resume", ExpectedSequence: expired.Sequence, ExpectedFence: expired.Fence})
	requireCode(t, err, "recovery_required")
	close(e.delayStop)
	<-run.done
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		active := len(s.active[session.ID])
		s.mu.Unlock()
		if active == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	commandTest(t, s, "alice", session.ID, Command{Type: "resume"})
	replacement := receiveRun(t, e)
	if replacement.req.Fence <= run.req.Fence || replacement.req.LeaseID == run.req.LeaseID {
		t.Fatal("takeover reused old fence or lease")
	}
	requireCode(t, run.emit(ExecutionEvent{Type: "tool.completed", Data: map[string]any{"name": "late"}}), "stale_fence")
	replacement.result <- ExecutionResult{Message: "finished"}
	waitStatus(t, s, "alice", session.ID, "idle")
}

func TestAdmissionBudgetsMultipleOwnersAndCompetingCoordinators(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	opts.MaxActive = 2
	opts.MaxActivePerOwner = 1
	s := openTest(t, opts)
	if other, err := Open(opts); err == nil {
		other.Close()
		t.Fatal("second coordinator stole store")
	}
	alice := createTest(t, s, "alice")
	alice2 := createTest(t, s, "alice")
	bob := createTest(t, s, "bob")
	charlie := createTest(t, s, "charlie")
	commandTest(t, s, "alice", alice.ID, Command{Type: "message", Text: "work"})
	a := receiveRun(t, e)
	_, err := s.Command(context.Background(), "alice", alice2.ID, Command{CommandID: "capacity-a", Type: "message", Text: "work", ExpectedSequence: alice2.Sequence, ExpectedFence: alice2.Fence})
	requireCode(t, err, "capacity")
	commandTest(t, s, "bob", bob.ID, Command{Type: "message", Text: "work"})
	b := receiveRun(t, e)
	_, err = s.Command(context.Background(), "charlie", charlie.ID, Command{CommandID: "capacity-c", Type: "message", Text: "work", ExpectedSequence: charlie.Sequence, ExpectedFence: charlie.Fence})
	requireCode(t, err, "capacity")
	if a.req.Owner == b.req.Owner || a.req.SessionID == b.req.SessionID || a.req.LeaseID == b.req.LeaseID {
		t.Fatal("execution owners/leases mixed")
	}
	a.result <- ExecutionResult{Message: "done"}
	waitStatus(t, s, "alice", alice.ID, "idle")
	commandTest(t, s, "charlie", charlie.ID, Command{Type: "message", Text: "work"})
	c := receiveRun(t, e)
	b.result <- ExecutionResult{}
	c.result <- ExecutionResult{}
	waitStatus(t, s, "bob", bob.ID, "idle")
	waitStatus(t, s, "charlie", charlie.ID, "idle")
}

func TestBoundedChildReviewAndParentStop(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	proposal := commandTest(t, s, "alice", session.ID, Command{Type: "child.request", Text: "Review tests"})
	select {
	case <-e.started:
		t.Fatal("proposed child executed without approval")
	default:
	}
	commandTest(t, s, "alice", session.ID, Command{Type: "child.review", ChildID: proposal.ChildID, Decision: "approve"})
	run := receiveRun(t, e)
	if !run.req.ReadOnly || run.req.ChildID != proposal.ChildID {
		t.Fatal("child authority not attenuated")
	}
	run.result <- ExecutionResult{Message: "Review passed"}
	<-run.done
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		cur, _ := s.Session("alice", session.ID)
		if cur.Children[0].Status == "succeeded" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	commandTest(t, s, "alice", session.ID, Command{Type: "child.review", ChildID: proposal.ChildID, Decision: "accept"})
	second := commandTest(t, s, "alice", session.ID, Command{Type: "child.request", Text: "Review again"})
	commandTest(t, s, "alice", session.ID, Command{Type: "child.review", ChildID: second.ChildID, Decision: "approve"})
	receiveRun(t, e)
	cur, _ := s.Session("alice", session.ID)
	_, err := s.Command(context.Background(), "alice", session.ID, Command{CommandID: "over-budget", Type: "child.request", Text: "third", ExpectedSequence: cur.Sequence, ExpectedFence: cur.Fence})
	requireCode(t, err, "child_limit")
	commandTest(t, s, "alice", session.ID, Command{Type: "stop"})
	stopped := waitStatus(t, s, "alice", session.ID, "stopped")
	if stopped.Children[1].Status != "cancelled" {
		t.Fatal("parent stop did not cancel child")
	}
}

func TestAuthWaitDoesNotCompleteOrAutomaticallyResume(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "ADO context"})
	run := receiveRun(t, e)
	run.result <- ExecutionResult{WaitingAuth: true, AuthReason: "connection expired"}
	waiting := waitStatus(t, s, "alice", session.ID, "waiting_auth")
	if waiting.Turns[0].Status != "waiting_auth" || waiting.ActiveTurnID == "" || waiting.Lease.Status != "released" {
		t.Fatal("auth wait falsely completed or retained compute")
	}
	events, _ := s.Events("alice", session.ID, 0)
	for _, event := range events {
		if event.Type == "turn.completed" {
			t.Fatal("auth wait emitted completion")
		}
	}
	commandTest(t, s, "alice", session.ID, Command{Type: "resume"})
	resumed := receiveRun(t, e)
	resumed.result <- ExecutionResult{WaitingAuth: true, AuthReason: "connection revoked"}
	waitStatus(t, s, "alice", session.ID, "waiting_auth")
}

func TestSplitStreamSecretsAndSensitiveEventFieldsAreScrubbed(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "check"})
	run := receiveRun(t, e)
	secret := "ghp_" + strings.Repeat("a", 36)
	for _, part := range []string{"hello " + secret[:10], secret[10:] + " world "} {
		if err := run.emit(ExecutionEvent{Type: "assistant.delta", Data: map[string]any{"text": part}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.emit(ExecutionEvent{Type: "tool.completed", Data: map[string]any{"accessToken": "opaque", "nested": map[string]any{"authorization": "opaque"}}}); err != nil {
		t.Fatal(err)
	}
	run.result <- ExecutionResult{Message: "done"}
	waitStatus(t, s, "alice", session.ID, "idle")
	b, _ := os.ReadFile(filepath.Join(s.opts.StateDir, "sessions.json"))
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "opaque") {
		t.Fatal("secret persisted")
	}
	if !strings.Contains(string(b), "REDACTED") {
		t.Fatal("redaction missing")
	}
}

func TestSurfacePolicyPinnedAcrossRestartAndStopHook(t *testing.T) {
	opts := options(t, newExecutor())
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	session := createTest(t, s, "alice")
	s.Close()
	opts.Surfaces[0].Instructions = "Expanded privileges"
	var fenced atomic.Bool
	opts.OnStop = func(context.Context, string, string) error { fenced.Store(true); return nil }
	next := openTest(t, opts)
	_, err = next.Command(context.Background(), "alice", session.ID, Command{CommandID: "policy", Type: "message", Text: "work", ExpectedSequence: session.Sequence, ExpectedFence: session.Fence})
	requireCode(t, err, "surface_changed")
	commandTest(t, next, "alice", session.ID, Command{Type: "stop"})
	if !fenced.Load() {
		t.Fatal("stop skipped authority fence")
	}
	waitStatus(t, next, "alice", session.ID, "stopped")
}

func TestOldTurnCannotPublishIntoNextTurnWithSameSessionFence(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "first"})
	first := receiveRun(t, e)
	first.result <- ExecutionResult{}
	waitStatus(t, s, "alice", session.ID, "idle")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "second"})
	second := receiveRun(t, e)
	if second.req.Fence != first.req.Fence {
		t.Fatal("test requires shared session authority fence")
	}
	requireCode(t, first.emit(ExecutionEvent{Type: "tool.completed", Data: map[string]any{"text": "stale"}}), "stale_fence")
	second.result <- ExecutionResult{}
	waitStatus(t, s, "alice", session.ID, "idle")
}

type interruptedEditor struct{}

func (interruptedEditor) Execute(ctx context.Context, req ExecutionRequest, emit func(ExecutionEvent) error) (ExecutionResult, error) {
	<-ctx.Done()
	return ExecutionResult{WorkspacePath: "/isolated/partial", Changes: []Change{{Path: "unfinished.go", Status: "added"}}, Diff: "+ partial work"}, ctx.Err()
}
func TestStopPreservesFirstTurnPartialChangesAfterWriterReturns(t *testing.T) {
	s := openTest(t, options(t, interruptedEditor{}))
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "edit"})
	commandTest(t, s, "alice", session.ID, Command{Type: "stop"})
	stopped := waitStatus(t, s, "alice", session.ID, "stopped")
	if stopped.WorkspacePath != "/isolated/partial" || stopped.Diff != "+ partial work" || len(stopped.Changes) != 1 {
		t.Fatalf("partial work lost: %+v", stopped)
	}
	events, _ := s.Events("alice", session.ID, 0)
	for _, event := range events {
		if event.Type == "turn.completed" {
			t.Fatal("cancelled work claimed successful completion")
		}
	}
}

func TestBrokerAdmissionChecksLeaseAndStopHookDoesNotInvertLocks(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	var s *Service
	var run *testExecution
	opts.OnStop = func(context.Context, string, string) error {
		returnError := s.CheckExecution("alice", run.req.SessionID, run.req.TurnID, run.req.LeaseID, run.req.Fence)
		requireCode(t, returnError, "stale_fence")
		return nil
	}
	s = openTest(t, opts)
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	run = receiveRun(t, e)
	if err := s.CheckExecution("alice", session.ID, run.req.TurnID, run.req.LeaseID, run.req.Fence); err != nil {
		t.Fatal(err)
	}
	requireCode(t, s.CheckExecution("bob", session.ID, run.req.TurnID, run.req.LeaseID, run.req.Fence), "not_found")
	requireCode(t, s.CheckExecution("alice", session.ID, run.req.TurnID, "another-lease", run.req.Fence), "stale_lease")
	commandTest(t, s, "alice", session.ID, Command{Type: "stop"})
	waitStatus(t, s, "alice", session.ID, "stopped")
}

func TestStopPropagationFailureRemainsPendingUntilRetried(t *testing.T) {
	opts := options(t, newExecutor())
	var calls int
	opts.OnStop = func(context.Context, string, string) error {
		calls++
		if calls == 1 {
			return errors.New("broker temporarily unavailable")
		}
		return nil
	}
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "stop"})
	pending := waitStatus(t, s, "alice", session.ID, "stopping")
	if !pending.StopFencePending {
		t.Fatal("failed broker fence was acknowledged effective")
	}
	commandTest(t, s, "alice", session.ID, Command{Type: "stop"})
	done := waitStatus(t, s, "alice", session.ID, "stopped")
	if done.StopFencePending {
		t.Fatal("successful fence retry not recorded")
	}
}

func TestStopAfterCrashWaitsForOrphanCustody(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	receiveRun(t, e)
	snapshot, err := os.ReadFile(filepath.Join(opts.StateDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	os.WriteFile(filepath.Join(opts.StateDir, "sessions.json"), snapshot, 0600)
	e2 := newExecutor()
	e2.recoverErr = errors.New("orphan worker could still write")
	opts.Executor = e2
	s2 := openTest(t, opts)
	commandTest(t, s2, "alice", session.ID, Command{Type: "stop"})
	pending := waitStatus(t, s2, "alice", session.ID, "stopping")
	if !pending.CustodyPending {
		t.Fatal("orphan writer custody guessed stopped")
	}
	events, _ := s2.Events("alice", session.ID, 0)
	for _, event := range events {
		if event.Type == "session.stopped" {
			t.Fatal("stop falsely acknowledged before custody confirmation")
		}
	}
	e2.recoverErr = nil
	commandTest(t, s2, "alice", session.ID, Command{Type: "stop"})
	waitStatus(t, s2, "alice", session.ID, "stopped")
}

func TestChildCompletionDoesNotRejectParentSteering(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	parentReceipt := commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	parent := receiveRun(t, e)
	childReceipt := commandTest(t, s, "alice", session.ID, Command{Type: "child.request", Text: "review"})
	commandTest(t, s, "alice", session.ID, Command{Type: "child.review", ChildID: childReceipt.ChildID, Decision: "approve"})
	child := receiveRun(t, e)
	steer := commandTest(t, s, "alice", session.ID, Command{Type: "steer", TurnID: parentReceipt.TurnID, Text: "adjust"})
	child.result <- ExecutionResult{}
	<-child.done
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		_, active := s.active[session.ID][child.req.ChildID]
		s.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.mu.Lock()
	status := s.state.Sessions[session.ID].Commands[steer.CommandID].Receipt.Status
	s.mu.Unlock()
	if status != "accepted" {
		t.Fatalf("child completion changed parent steering to %s", status)
	}
	(<-parent.req.Steering).Ack(nil)
	parent.result <- ExecutionResult{}
	waitStatus(t, s, "alice", session.ID, "idle")
}

func TestTypedToolResultsAndPinnedInstructionsAreSanitized(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	secret := "ghp_" + strings.Repeat("b", 36)
	opts.Surfaces[0].Instructions = "Never reveal " + secret
	s := openTest(t, opts)
	opts.Surfaces[0].Instructions = "Caller mutation must not alter admitted configuration"
	session := createTest(t, s, "alice")
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	run := receiveRun(t, e)
	if strings.Contains(run.req.Instructions, secret) || strings.Contains(run.req.Instructions, "Caller mutation") {
		t.Fatal("surface instructions not copied and sanitized")
	}
	value := struct {
		AccessToken string `json:"accessToken"`
		Nested      string `json:"nested"`
	}{"opaque-private", secret}
	if err := run.emit(ExecutionEvent{Type: "tool.completed", Data: map[string]any{"result": value}}); err != nil {
		t.Fatal(err)
	}
	if err := run.emit(ExecutionEvent{Type: "tool.completed", Data: map[string]any{"invalid": make(chan int)}}); err == nil {
		t.Fatal("non-JSON output accepted")
	}
	run.result <- ExecutionResult{}
	waitStatus(t, s, "alice", session.ID, "idle")
	bytes, _ := os.ReadFile(filepath.Join(s.opts.StateDir, "sessions.json"))
	if strings.Contains(string(bytes), secret) || strings.Contains(string(bytes), "opaque-private") {
		t.Fatal("typed payload bypassed scrubber")
	}
}

func TestOutputBudgetRefusesFurtherOutputWithoutLosingReceipts(t *testing.T) {
	e := newExecutor()
	opts := options(t, e)
	opts.MaxSessionOutputBytes = 256
	s := openTest(t, opts)
	session := createTest(t, s, "alice")
	receipt := commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "bounded"})
	run := receiveRun(t, e)
	requireCode(t, run.emit(ExecutionEvent{Type: "tool.completed", Data: map[string]any{"text": strings.Repeat("output ", 100)}}), "capacity")
	run.result <- ExecutionResult{Message: "output limit reached"}
	completed := waitStatus(t, s, "alice", session.ID, "idle")
	s.mu.Lock()
	saved := s.state.Sessions[session.ID].Commands[receipt.CommandID]
	fatal := s.fatal
	s.mu.Unlock()
	if saved.Receipt.CommandID != receipt.CommandID || fatal != nil || completed.OutputBytes != 0 {
		t.Fatal("output pressure damaged command/custody ledger")
	}
}
