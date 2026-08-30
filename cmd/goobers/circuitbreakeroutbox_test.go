package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// circuitBreakerFakeCommenter is blockedHandlerFakeCommenter with the park
// mutation separately faultable: #3646's scenario is a healthy comment path
// (the failure streak still counts) whose label mutation fails, which is
// exactly the case whose error used to be discarded.
type circuitBreakerFakeCommenter struct {
	calls    []providers.UpdateWorkItemRequest
	comments []providers.Comment
	nextID   int
	parkErr  error
}

func (f *circuitBreakerFakeCommenter) ListComments(_ context.Context, _ providers.RepositoryRef, _ string) ([]providers.Comment, error) {
	return append([]providers.Comment(nil), f.comments...), nil
}

func (f *circuitBreakerFakeCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.calls = append(f.calls, req)
	if len(req.AddLabels) > 0 || len(req.RemoveLabels) > 0 {
		if f.parkErr != nil {
			return providers.WorkItem{}, f.parkErr
		}
	}
	if req.Comment != "" {
		f.nextID++
		f.comments = append(f.comments, providers.Comment{ID: strconv.Itoa(f.nextID), Body: req.Comment})
	}
	return providers.WorkItem{}, nil
}

func (f *circuitBreakerFakeCommenter) UpdateComment(_ context.Context, _ providers.RepositoryRef, commentID, body string) error {
	for i, c := range f.comments {
		if c.ID == commentID {
			f.comments[i].Body = body
			return nil
		}
	}
	return errors.New("comment not found")
}

func (f *circuitBreakerFakeCommenter) parkCalls() []providers.UpdateWorkItemRequest {
	var parks []providers.UpdateWorkItemRequest
	for _, call := range f.calls {
		if slices.Contains(call.AddLabels, providers.LabelNeedsHuman) {
			parks = append(parks, call)
		}
	}
	return parks
}

func circuitBreakerTestLayout(t *testing.T) instance.Layout {
	t.Helper()
	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	return l
}

func seedCircuitBreakerClaim(t *testing.T, l instance.Layout, itemID, runID string) {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim(itemID, runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim %s/%s: ok=%v err=%v", itemID, runID, ok, err)
	}
}

func circuitBreakerTestRepo() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
}

func installCircuitBreakerFake(t *testing.T, fake gate.Commenter) {
	t.Helper()
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })
}

func circuitBreakerTestConfig() *instance.Config {
	return &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
}

// TestCircuitBreakerParkFailurePersistedAndSurfaced covers #3646's core
// regression: when the park mutation fails, the notifier wrapper surfaces the
// error to the runner instead of discarding it, and the owed mutation is
// recorded durably in the circuit-breaker outbox.
func TestCircuitBreakerParkFailurePersistedAndSurfaced(t *testing.T) {
	l := circuitBreakerTestLayout(t)
	seedCircuitBreakerClaim(t, l, "41", "run-cb-fail")
	fake := &circuitBreakerFakeCommenter{parkErr: errors.New("provider unreachable")}
	installCircuitBreakerFake(t, fake)

	h := buildTerminalCircuitBreaker(l, circuitBreakerTestConfig(), blockedHandlerTestResolver(t), &escTestRegistrar{}, nil)
	var lastErr error
	for i := 0; i < failureStreakThreshold; i++ {
		lastErr = h("run-cb-fail", journal.PhaseEscalated, "open-pr-gate")
	}
	if lastErr == nil {
		t.Fatal("terminal notifier returned nil despite the park mutation failing")
	}

	pending, err := snapshotCircuitBreakerOutbox(l)
	if err != nil {
		t.Fatalf("snapshotCircuitBreakerOutbox: %v", err)
	}
	entry, ok := pending[blockedRecordKey(circuitBreakerTestRepo(), "41")]
	if !ok {
		t.Fatalf("no outbox entry recorded for the failed park, got %+v", pending)
	}
	if entry.ItemID != "41" || entry.RunID != "run-cb-fail" {
		t.Fatalf("outbox entry = %+v, want item 41 from run-cb-fail", entry)
	}
	if entry.Attempts < 1 || entry.LastError == "" {
		t.Fatalf("outbox entry = %+v, want an attempt count and a retained diagnostic", entry)
	}
	if entry.FailureStreak < failureStreakThreshold {
		t.Fatalf("outbox entry streak = %d, want at least %d", entry.FailureStreak, failureStreakThreshold)
	}
	if _, err := os.Stat(circuitBreakerOutboxPath(l)); err != nil {
		t.Fatalf("outbox file not persisted: %v", err)
	}
}

// TestCircuitBreakerOutboxReconciledOnNextTerminal proves the persisted park is
// retried — and dropped once it lands — rather than staying owed forever.
func TestCircuitBreakerOutboxReconciledOnNextTerminal(t *testing.T) {
	l := circuitBreakerTestLayout(t)
	seedCircuitBreakerClaim(t, l, "41", "run-cb-fail")
	fake := &circuitBreakerFakeCommenter{parkErr: errors.New("provider unreachable")}
	installCircuitBreakerFake(t, fake)

	h := buildTerminalCircuitBreaker(l, circuitBreakerTestConfig(), blockedHandlerTestResolver(t), &escTestRegistrar{}, nil)
	for i := 0; i < failureStreakThreshold; i++ {
		_ = h("run-cb-fail", journal.PhaseEscalated, "open-pr-gate")
	}
	if pending, err := snapshotCircuitBreakerOutbox(l); err != nil || len(pending) != 1 {
		t.Fatalf("outbox = %+v (err %v), want the failed park pending", pending, err)
	}

	fake.parkErr = nil
	fake.calls = nil
	seedCircuitBreakerClaim(t, l, "42", "run-cb-next")
	if err := h("run-cb-next", journal.PhaseAborted, "abort-gate"); err != nil {
		t.Fatalf("terminal notifier: %v", err)
	}

	parks := fake.parkCalls()
	var retried bool
	for _, park := range parks {
		if park.ID == "41" {
			retried = true
			if !slices.Contains(park.RemoveLabels, providers.LabelReady) {
				t.Fatalf("retried park = %+v, want it to remove %s", park, providers.LabelReady)
			}
		}
	}
	if !retried {
		t.Fatalf("pending park for item 41 was never retried, calls = %+v", fake.calls)
	}

	pending, err := snapshotCircuitBreakerOutbox(l)
	if err != nil {
		t.Fatalf("snapshotCircuitBreakerOutbox: %v", err)
	}
	if _, ok := pending[blockedRecordKey(circuitBreakerTestRepo(), "41")]; ok {
		t.Fatalf("a landed retry must drop its outbox entry, got %+v", pending)
	}
}

// TestCircuitBreakerOutboxClearedByCompletedTerminal proves a completed run
// drops the pending park for its items: the streak that motivated the park is
// reset, so the owed mutation is moot rather than replayed later.
func TestCircuitBreakerOutboxClearedByCompletedTerminal(t *testing.T) {
	l := circuitBreakerTestLayout(t)
	seedCircuitBreakerClaim(t, l, "43", "run-cb-ok")
	repo := circuitBreakerTestRepo()
	if err := recordCircuitBreakerMutationFailure(l, repo, "43", "run-cb-old", "implement", failureStreakThreshold, errors.New("provider unreachable")); err != nil {
		t.Fatalf("recordCircuitBreakerMutationFailure: %v", err)
	}

	fake := &circuitBreakerFakeCommenter{}
	installCircuitBreakerFake(t, fake)
	h := buildTerminalCircuitBreaker(l, circuitBreakerTestConfig(), blockedHandlerTestResolver(t), &escTestRegistrar{}, nil)
	if err := h("run-cb-ok", journal.PhaseCompleted, "done"); err != nil {
		t.Fatalf("terminal notifier: %v", err)
	}

	pending, err := snapshotCircuitBreakerOutbox(l)
	if err != nil {
		t.Fatalf("snapshotCircuitBreakerOutbox: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("completed terminal left %d pending park(s), want none: %+v", len(pending), pending)
	}
	if len(fake.parkCalls()) != 0 {
		t.Fatalf("a completed terminal must not park anything, got %+v", fake.calls)
	}
}

// TestCircuitBreakerOutboxSuccessLeavesNoResidue pins the unchanged happy path:
// a park that lands writes nothing to the outbox.
func TestCircuitBreakerOutboxSuccessLeavesNoResidue(t *testing.T) {
	l := circuitBreakerTestLayout(t)
	seedCircuitBreakerClaim(t, l, "44", "run-cb-good")
	fake := &circuitBreakerFakeCommenter{}
	installCircuitBreakerFake(t, fake)

	h := buildTerminalCircuitBreaker(l, circuitBreakerTestConfig(), blockedHandlerTestResolver(t), &escTestRegistrar{}, nil)
	for i := 0; i < failureStreakThreshold; i++ {
		if err := h("run-cb-good", journal.PhaseEscalated, "open-pr-gate"); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if len(fake.parkCalls()) == 0 {
		t.Fatal("circuit breaker did not fire after the threshold")
	}
	pending, err := snapshotCircuitBreakerOutbox(l)
	if err != nil {
		t.Fatalf("snapshotCircuitBreakerOutbox: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a landed park must leave the outbox empty, got %+v", pending)
	}
}

// TestCircuitBreakerOutboxBoundsEntries proves a long provider outage cannot
// grow the outbox without limit, and never evicts the newest recording.
func TestCircuitBreakerOutboxBoundsEntries(t *testing.T) {
	pending := map[string]circuitBreakerMutation{}
	base := time.Now().UTC()
	for i := 0; i < maxCircuitBreakerOutboxEntries+10; i++ {
		key := "acme/web#" + strconv.Itoa(i)
		pending[key] = circuitBreakerMutation{ItemID: strconv.Itoa(i), RecordedAt: base.Add(time.Duration(i) * time.Second)}
	}
	newest := "acme/web#" + strconv.Itoa(maxCircuitBreakerOutboxEntries+9)
	evictOldestCircuitBreakerMutations(pending, newest)

	if len(pending) != maxCircuitBreakerOutboxEntries {
		t.Fatalf("outbox size = %d, want %d", len(pending), maxCircuitBreakerOutboxEntries)
	}
	if _, ok := pending[newest]; !ok {
		t.Fatal("the newest recording must never be evicted")
	}
	if _, ok := pending["acme/web#0"]; ok {
		t.Fatal("the oldest recording should have been evicted first")
	}
}

// TestCircuitBreakerOutboxSurvivesCorruptFile proves a malformed outbox is a
// reported error, not a silent no-op that would lose every owed park.
func TestCircuitBreakerOutboxSurvivesCorruptFile(t *testing.T) {
	l := circuitBreakerTestLayout(t)
	if err := os.WriteFile(circuitBreakerOutboxPath(l), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt outbox: %v", err)
	}
	if _, err := snapshotCircuitBreakerOutbox(l); err == nil {
		t.Fatal("expected a parse error for a corrupt outbox file")
	}
}

// TestComposeTerminalNotifierPropagatesCircuitBreakerError covers the daemon
// wiring boundary: the composed NotifyTerminal used to discard the circuit
// breaker's error, so a failed park never reached the runner and never landed
// as terminal_notification_failed in a real deployment (#3646).
func TestComposeTerminalNotifierPropagatesCircuitBreakerError(t *testing.T) {
	breakerErr := errors.New("park failed")
	notifierCalls := 0
	composed := composeTerminalNotifier(
		func(string, journal.RunPhase, string) error { return breakerErr },
		func(string, journal.RunPhase, string) error {
			notifierCalls++
			return nil
		},
	)

	err := composed("run-1", journal.PhaseCompleted, "failure")
	if !errors.Is(err, breakerErr) {
		t.Fatalf("composed notifier error = %v, want circuit-breaker error %v", err, breakerErr)
	}
	if notifierCalls != 1 {
		t.Fatalf("terminal notifier calls = %d, want 1: a breaker failure must not suppress the notification", notifierCalls)
	}
}

// TestComposeTerminalNotifierJoinsBothErrors asserts neither half's failure
// masks the other, so both land in the journaled diagnostic (#3646).
func TestComposeTerminalNotifierJoinsBothErrors(t *testing.T) {
	breakerErr := errors.New("park failed")
	notifyErr := errors.New("notify failed")
	composed := composeTerminalNotifier(
		func(string, journal.RunPhase, string) error { return breakerErr },
		func(string, journal.RunPhase, string) error { return notifyErr },
	)

	err := composed("run-1", journal.PhaseCompleted, "failure")
	if !errors.Is(err, breakerErr) || !errors.Is(err, notifyErr) {
		t.Fatalf("composed notifier error = %v, want both %v and %v", err, breakerErr, notifyErr)
	}
}

// TestComposeTerminalNotifierPassesThroughSingleHook keeps the existing
// behavior for the wirings where only one of the two hooks is configured.
func TestComposeTerminalNotifierPassesThroughSingleHook(t *testing.T) {
	if got := composeTerminalNotifier(nil, nil); got != nil {
		t.Fatalf("composeTerminalNotifier(nil, nil) = non-nil, want nil")
	}

	breakerCalls := 0
	breakerOnly := composeTerminalNotifier(func(string, journal.RunPhase, string) error {
		breakerCalls++
		return nil
	}, nil)
	if err := breakerOnly("run-1", journal.PhaseCompleted, "success"); err != nil {
		t.Fatalf("breaker-only notifier: %v", err)
	}
	if breakerCalls != 1 {
		t.Fatalf("breaker calls = %d, want 1", breakerCalls)
	}

	notifyCalls := 0
	notifierOnly := composeTerminalNotifier(nil, func(string, journal.RunPhase, string) error {
		notifyCalls++
		return nil
	})
	if err := notifierOnly("run-1", journal.PhaseCompleted, "success"); err != nil {
		t.Fatalf("notifier-only notifier: %v", err)
	}
	if notifyCalls != 1 {
		t.Fatalf("notifier calls = %d, want 1", notifyCalls)
	}
}

// TestComposeTerminalNotifierSuccessReturnsNil pins that the healthy path is
// unchanged: both hooks run and the composed notifier reports no error.
func TestComposeTerminalNotifierSuccessReturnsNil(t *testing.T) {
	order := make([]string, 0, 2)
	composed := composeTerminalNotifier(
		func(string, journal.RunPhase, string) error {
			order = append(order, "breaker")
			return nil
		},
		func(string, journal.RunPhase, string) error {
			order = append(order, "notify")
			return nil
		},
	)

	if err := composed("run-1", journal.PhaseCompleted, "success"); err != nil {
		t.Fatalf("composed notifier error = %v, want nil", err)
	}
	if !slices.Equal(order, []string{"breaker", "notify"}) {
		t.Fatalf("hook order = %v, want [breaker notify]", order)
	}
}
