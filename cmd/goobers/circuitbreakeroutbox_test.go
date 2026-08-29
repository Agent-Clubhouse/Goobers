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
