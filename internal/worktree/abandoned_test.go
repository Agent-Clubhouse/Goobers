package worktree

import (
	"context"
	"errors"
	"os"
	"testing"
)

// createActiveWorktree returns a worktree in the exact shape #5035 leaks:
// marker status `active`, owning PID this live test process. The crash
// reaper must skip it on liveness alone, so whatever Reap does with it is
// decided entirely by the abandoned check.
func createActiveWorktree(t *testing.T, ctx context.Context, m *Manager, repo, runID, ownerRunID string) *Worktree {
	t.Helper()
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: runID, OwnerRunID: ownerRunID,
		BaseRef: "main", Branch: "goobers/test/" + runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	mk, err := readMarker(m.markerPath(wt.key, wt.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if mk.Status != statusActive {
		t.Fatalf("fixture marker status = %q, want %q", mk.Status, statusActive)
	}
	if !processAlive(mk.PID) || pidReused(mk) {
		t.Fatalf("fixture owner PID %d must be alive and unreused for this test to mean anything", mk.PID)
	}
	return wt
}

func TestReapRemovesActiveWorktreeWhoseRunSettled(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt := createActiveWorktree(t, ctx, m, repo, "settled-stage", "settled-owner")

	var gotWorktreeID, gotOwnerRunID string
	results, warnings, err := m.Reap(ctx, ReapOptions{
		IsRunAbandoned: func(worktreeID, ownerRunID string) (bool, error) {
			gotWorktreeID, gotOwnerRunID = worktreeID, ownerRunID
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	if len(results) != 1 || results[0].Reason != ReapReasonAbandoned {
		t.Fatalf("results = %+v, want one %q", results, ReapReasonAbandoned)
	}
	// The doc comment claims the stamped owner is forwarded so the journal
	// resolves exactly rather than by prefix. Assert it, or that claim rots.
	if gotWorktreeID != "settled-stage" || gotOwnerRunID != "settled-owner" {
		t.Fatalf("abandoned check saw (%q, %q), want (settled-stage, settled-owner)", gotWorktreeID, gotOwnerRunID)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("abandoned worktree still on disk: %v", err)
	}
	if _, err := os.Stat(m.markerPath(wt.key, wt.RunID)); !os.IsNotExist(err) {
		t.Fatalf("abandoned marker still on disk: %v", err)
	}
}

func TestReapLeavesActiveWorktreeWhoseRunIsStillRunning(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt := createActiveWorktree(t, ctx, m, repo, "live-stage", "live-owner")

	results, warnings, err := m.Reap(ctx, ReapOptions{
		IsRunAbandoned: func(string, string) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(results) != 0 || len(warnings) != 0 {
		t.Fatalf("results=%+v warnings=%+v, want a live run's worktree untouched", results, warnings)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("reap deleted a live run's worktree: %v", err)
	}
}

// A nil IsRunAbandoned must behave exactly as Reap did before #5035. This is
// also the regression guard for the leak itself: it pins the pre-fix
// behaviour that a settled run's active marker survives, so the fix above
// can only ever be an opt-in.
func TestReapWithoutAbandonedCheckLeavesActiveWorktree(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt := createActiveWorktree(t, ctx, m, repo, "unchecked-stage", "unchecked-owner")

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(results) != 0 || len(warnings) != 0 {
		t.Fatalf("results=%+v warnings=%+v, want nil check to change nothing", results, warnings)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("nil abandoned check still deleted the worktree: %v", err)
	}
}

// An unreadable owning journal must not delete the worktree and must not
// abort the pass — same contract every other per-marker failure here has.
func TestReapReportsAbandonedCheckFailureAsWarning(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt := createActiveWorktree(t, ctx, m, repo, "unreadable-stage", "unreadable-owner")
	other := createActiveWorktree(t, ctx, m, repo, "second-stage", "second-owner")

	probeErr := errors.New("read run phase: permission denied")
	results, warnings, err := m.Reap(ctx, ReapOptions{
		IsRunAbandoned: func(worktreeID, _ string) (bool, error) {
			if worktreeID == "unreadable-stage" {
				return false, probeErr
			}
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("Reap aborted the whole pass on one unreadable owner: %v", err)
	}
	if len(warnings) != 1 || !errors.Is(warnings[0].Err, probeErr) {
		t.Fatalf("warnings = %+v, want one carrying the probe error", warnings)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("unresolvable owner must not delete the worktree: %v", err)
	}
	// The other worktree still had to be reaped: one bad probe cannot stop
	// the rest of the sweep.
	if len(results) != 1 || results[0].RunID != "second-stage" {
		t.Fatalf("results = %+v, want the second worktree still reaped", results)
	}
	if _, err := os.Stat(other.Path); !os.IsNotExist(err) {
		t.Fatalf("second abandoned worktree survived: %v", err)
	}
}
