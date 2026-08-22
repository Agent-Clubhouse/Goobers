package localscheduler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLiveness maps runID -> answer for RunLive; missing runIDs answer
// (false, nil) — definitively not live.
type fakeLiveness struct {
	live map[string]bool
	errs map[string]error
	seen []string
}

func (f *fakeLiveness) RunLive(_ context.Context, runID string) (bool, error) {
	f.seen = append(f.seen, runID)
	if err := f.errs[runID]; err != nil {
		return false, err
	}
	return f.live[runID], nil
}

func TestTrackedRunLivenessReEvaluatesPerCall(t *testing.T) {
	tracked := []string{}
	probe := TrackedRunLiveness(func() []string { return tracked })

	if live, err := probe.RunLive(context.Background(), "run-a"); err != nil || live {
		t.Fatalf("untracked run: live=%v err=%v, want not live", live, err)
	}
	tracked = []string{"run-a"}
	if live, err := probe.RunLive(context.Background(), "run-a"); err != nil || !live {
		t.Fatalf("later-tracked run: live=%v err=%v, want live without rebuilding the probe", live, err)
	}
}

func TestCompositeRunLivenessAnyProbeAnswersLive(t *testing.T) {
	dead := &fakeLiveness{}
	alive := &fakeLiveness{live: map[string]bool{"run-a": true}}
	failing := &fakeLiveness{errs: map[string]error{"run-a": errors.New("frontend unreachable"), "run-b": errors.New("frontend unreachable")}}

	if live, err := CompositeRunLiveness(dead, alive).RunLive(context.Background(), "run-a"); err != nil || !live {
		t.Fatalf("dead+alive: live=%v err=%v, want live", live, err)
	}
	// An earlier probe's error must not mask a later probe's live answer.
	if live, err := CompositeRunLiveness(failing, alive).RunLive(context.Background(), "run-a"); err != nil || !live {
		t.Fatalf("failing+alive: live=%v err=%v, want live with nil error", live, err)
	}
	// No live answer anywhere: errors surface so the caller can fail live.
	if live, err := CompositeRunLiveness(failing, dead).RunLive(context.Background(), "run-b"); live || err == nil {
		t.Fatalf("failing+dead: live=%v err=%v, want unknown (error)", live, err)
	}
	// All definitive not-live: no error.
	if live, err := CompositeRunLiveness(dead).RunLive(context.Background(), "run-b"); live || err != nil {
		t.Fatalf("dead only: live=%v err=%v, want definitively not live", live, err)
	}
}

func TestProbeLiveClaimHoldersDedupesAndFailsLiveOnError(t *testing.T) {
	entries := []ClaimEntry{
		{ItemID: "issue-1", RunID: "live-run"},
		{ItemID: "issue-2", RunID: "live-run"}, // duplicate holder: probed once
		{ItemID: "issue-3", RunID: "dead-run"},
		{ItemID: "issue-4", RunID: "unknown-run"},
	}
	probe := &fakeLiveness{
		live: map[string]bool{"live-run": true},
		errs: map[string]error{"unknown-run": errors.New("describe timeout")},
	}

	live, err := ProbeLiveClaimHolders(context.Background(), entries, probe)
	if len(probe.seen) != 3 {
		t.Fatalf("probed runs = %v, want each distinct holder exactly once", probe.seen)
	}
	if !live["live-run"] {
		t.Fatal("live-run must be in the live set")
	}
	if live["dead-run"] {
		t.Fatal("dead-run answered definitively not live and must not be renewed")
	}
	// DS6: only a closed-or-vanished workflow lets a lease lapse. A probe
	// error is neither, so the run fails LIVE and the error is surfaced.
	if !live["unknown-run"] {
		t.Fatal("unknown-run (probe error) must fail live")
	}
	if err == nil || !strings.Contains(err.Error(), "unknown-run") {
		t.Fatalf("err = %v, want the fail-live degradation reported", err)
	}
}

func TestRenewRunsRenewsOnlyLiveHoldersFromCurrentEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	now := start
	ledger, err := OpenClaimLedger(path, WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	for item, run := range map[string]string{"issue-1": "live-run", "issue-2": "dead-run"} {
		if ok, _, err := ledger.Claim(item, run, "implementation", 30*time.Minute); err != nil || !ok {
			t.Fatalf("seed %s: ok=%v err=%v", item, ok, err)
		}
	}
	// A holder in the live set whose claim was RELEASED between the caller's
	// liveness snapshot and this renewal must not be resurrected.
	if ok, _, err := ledger.Claim("issue-3", "released-run", "implementation", 30*time.Minute); err != nil || !ok {
		t.Fatalf("seed issue-3: ok=%v err=%v", ok, err)
	}
	if err := ledger.Release("issue-3", "released-run"); err != nil {
		t.Fatal(err)
	}

	now = start.Add(20 * time.Minute)
	renewed, err := ledger.RenewRuns(map[string]bool{"live-run": true, "released-run": true}, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(renewed) != 1 || renewed[0].ItemID != "issue-1" {
		t.Fatalf("renewed = %+v, want exactly live-run's issue-1", renewed)
	}
	liveEntry, held := ledger.Lookup("issue-1")
	if !held || !liveEntry.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("issue-1 = %+v held=%v, want lease extended from renewal time", liveEntry, held)
	}
	deadEntry, held := ledger.Lookup("issue-2")
	if !held || !deadEntry.ExpiresAt.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("issue-2 = %+v held=%v, want lease untouched so it lapses normally", deadEntry, held)
	}
	if _, held := ledger.Lookup("issue-3"); held {
		t.Fatal("released claim must not be resurrected by renewal")
	}
}

// TestRecoveryGateBlocksUntilRenewalRebuilt is DS6's load-bearing ordering
// primitive (distributed-state-and-coordination.md §10): recovery is refused
// until the renewal set has been rebuilt, and a nil gate — the one-shot
// non-daemon callers — always permits.
func TestRecoveryGateBlocksUntilRenewalRebuilt(t *testing.T) {
	gate := NewRecoveryGate()
	if gate.RecoveryPermitted() {
		t.Fatal("a fresh gate must refuse recovery until the renewal set is rebuilt")
	}
	gate.MarkRenewalRebuilt()
	if !gate.RecoveryPermitted() {
		t.Fatal("a rebuilt gate must permit recovery")
	}
	gate.MarkRenewalRebuilt() // idempotent
	if !gate.RecoveryPermitted() {
		t.Fatal("marking rebuilt twice must stay open")
	}
	var nilGate *RecoveryGate
	if !nilGate.RecoveryPermitted() {
		t.Fatal("a nil gate must permit recovery (callers with no renewal duty)")
	}
}
