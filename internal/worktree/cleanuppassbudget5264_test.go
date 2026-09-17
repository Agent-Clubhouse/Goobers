package worktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// seedPendingPopulation creates a synthetic population of cleanup-pending
// worktrees and returns their handles.
//
// Remove only leaves behind the durable pending marker — the record pair that IS
// the retry queue — while a guard refuses the handoff. So the population has to
// be created under a refusing guard, and each test installs its own guard
// afterwards (SetCleanupGuard replaces by name). Creating the population under
// the test's real guard instead would clean each entry up immediately and leave
// nothing to discover.
func seedPendingPopulation(t *testing.T, ctx context.Context, m *Manager, repo string, runIDs []string) []*Worktree {
	t.Helper()
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return errors.New("seeding: defer cleanup")
	}); err != nil {
		t.Fatal(err)
	}
	worktrees := make([]*Worktree, 0, len(runIDs))
	for _, runID := range runIDs {
		worktrees = append(worktrees, createPendingRetryWorktree(t, ctx, m, repo, runID, "owner"))
	}
	return worktrees
}

// steppingClock returns a clock that advances by step on every read, starting
// from a fixed epoch.
//
// A fake clock, not a real one, because the thing under test is a wall-clock
// budget: a test that proved truncation by doing enough real work to exceed a
// real duration would be precisely the wall-clock assertion that turns flaky
// the first time CI is under contention. Advancing per read also makes the
// truncation point exact rather than probabilistic.
func steppingClock(step time.Duration) func() time.Time {
	base := time.Unix(1_700_000_000, 0).UTC()
	reads := 0
	return func() time.Time {
		now := base.Add(time.Duration(reads) * step)
		reads++
		return now
	}
}

// TestCleanupPassBudgetStopsDiscoveryAndReportsDeferral is #5264's core
// acceptance for the whole-pass bound: the budget covers DISCOVERY, not just
// attempts, and a pass that stops early says so instead of presenting itself as
// a completed cleanup.
//
// Before this, opts.Limit bounded attempted cleanups while discovery read every
// durable record under Root first — so with a large retained population (#5214:
// 1,590 terminal worktrees) the scan itself was the unbounded cost, on a path
// that startup and stale preparation both wait behind.
func TestCleanupPassBudgetStopsDiscoveryAndReportsDeferral(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	seedPendingPopulation(t, ctx, m, repo, []string{"run-a", "run-b", "run-c", "run-d", "run-e"})

	// The budget is exhausted on the 3rd clock read: read 0 is the pass start,
	// so discovery can examine two records before the bound trips.
	const step = time.Second
	report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{
		Limit: 5, PassBudget: 3 * step, Clock: steppingClock(step),
	})
	if err != nil {
		t.Fatalf("RetryCleanupPending: %v", err)
	}
	if !report.Deferred {
		t.Fatalf("report.Deferred = false, want true; report = %+v", report)
	}
	if report.DeferralReason != CleanupDeferralBudgetDiscovery {
		t.Errorf("DeferralReason = %q, want %q", report.DeferralReason, CleanupDeferralBudgetDiscovery)
	}
	// Progress reporting: the pass examined real records and says how many,
	// which is what lets an operator see work happening on a pass that removed
	// nothing.
	if report.Examined == 0 || report.Examined >= 5 {
		t.Errorf("Examined = %d, want a partial count (>0, <5)", report.Examined)
	}
	if report.Elapsed <= 0 {
		t.Errorf("Elapsed = %v, want a measured duration", report.Elapsed)
	}
	if report.Attempted > report.Examined {
		t.Errorf("Attempted = %d exceeds Examined = %d", report.Attempted, report.Examined)
	}
	// The deferral is also surfaced as a warning, because the daemon caller
	// aggregates warnings and would otherwise report a silent short pass.
	if !hasWarningContaining(report.Warnings, string(CleanupDeferralBudgetDiscovery)) {
		t.Errorf("warnings do not name the deferral: %v", report.Warnings)
	}

	// Unprocessed records survive: they are the durable queue, so a bounded
	// pass must never consume them.
	for _, runID := range []string{"run-a", "run-b", "run-c", "run-d", "run-e"} {
		if _, err := os.Stat(m.markerPath(keyForTestRepo(t, m), runID)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("unexpected marker error for %s: %v", runID, err)
		}
	}
}

// TestCleanupDiscoveryLimitTruncatesButProgressesAcrossPasses is the fairness
// half. Truncating discovery is only safe because the scan is ordered and
// resumes after the caller's cursor: a truncated pass must make FORWARD
// progress, not re-read the same prefix forever.
//
// One entry is permanently failing (the guard always refuses it), which is the
// "large population containing one locked or permanently failing entry" case —
// it must not starve the others.
func TestCleanupDiscoveryLimitTruncatesButProgressesAcrossPasses(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	const stuck = "run-03"
	runIDs := []string{"run-01", "run-02", "run-03", "run-04", "run-05", "run-06"}
	seedPendingPopulation(t, ctx, m, repo, runIDs)

	// Now install the real guard: it clears every entry except the one that is
	// permanently blocked.
	attempts := map[string]int{}
	if err := m.SetCleanupGuard("recovery", func(_ context.Context, target CleanupTarget) error {
		attempts[target.WorktreeID]++
		if strings.Contains(target.WorktreeID, stuck) {
			return errors.New("permanently blocked entry")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Two attempts and a discovery bound of two per pass: every pass is
	// truncated, so only a cursor-resuming scan can ever reach run-06.
	cursor := ""
	seen := map[string]bool{}
	for pass := 0; pass < len(runIDs); pass++ {
		report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{
			After: cursor, Limit: 2, DiscoveryLimit: 2,
		})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if report.Examined == 0 {
			t.Fatalf("pass %d examined nothing; report = %+v", pass, report)
		}
		cursor = report.Next
		for _, removed := range report.Removed {
			seen[removed.RunID] = true
		}
	}

	// Every entry except the permanently failing one got cleaned up, which is
	// the "other eligible entries progress fairly" requirement.
	for _, runID := range runIDs {
		if runID == stuck {
			continue
		}
		if !seen[runID] {
			t.Errorf("%s never progressed across bounded passes (removed: %v)", runID, seen)
		}
	}
	if seen[stuck] {
		t.Errorf("%s was removed despite its guard refusing", stuck)
	}
	// Its durable record survives, so the operator still has the evidence and a
	// later pass can retry it.
	if attempts[stuck] == 0 {
		t.Errorf("the permanently failing entry was never attempted: %v", attempts)
	}
}

// TestCleanupPassBudgetZeroIsUnbounded pins the compatibility promise: a caller
// that sets no budget behaves exactly as before, so this change cannot alter any
// existing cleanup path by being merely present.
func TestCleanupPassBudgetZeroIsUnbounded(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	seedPendingPopulation(t, ctx, m, repo, []string{"run-x", "run-y", "run-z"})
	// Clearing guard: with the handoff satisfied, an unbudgeted pass must drain
	// the whole queue.
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 8})
	if err != nil {
		t.Fatalf("RetryCleanupPending: %v", err)
	}
	if report.Deferred {
		t.Errorf("an unbudgeted pass reported a deferral: %+v", report)
	}
	if report.Attempted != 3 || len(report.Removed) != 3 {
		t.Errorf("report = %+v, want all three attempted and removed", report)
	}
	if report.Examined != 3 {
		t.Errorf("Examined = %d, want 3", report.Examined)
	}
}

// TestCleanupPassBudgetNeverInterruptsAnAttempt pins the ordering rule that
// keeps the bound safe: the budget is consulted before an attempt STARTS, never
// during one. A cleanup in flight holds the repository lock and has its durable
// record pair mid-transition, so abandoning it partway is how the two records
// would be left disagreeing — the exact condition the ownership re-read exists
// to refuse.
func TestCleanupPassBudgetNeverInterruptsAnAttempt(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt := seedPendingPopulation(t, ctx, m, repo, []string{"run-single"})[0]
	var guardCalls int
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		guardCalls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A budget already spent before the pass begins: the attempt loop must
	// decline to start, leaving the durable records exactly as they were.
	report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{
		Limit: 1, PassBudget: time.Nanosecond, Clock: steppingClock(time.Second),
	})
	if err != nil {
		t.Fatalf("RetryCleanupPending: %v", err)
	}
	if report.Attempted != 0 || len(report.Removed) != 0 {
		t.Fatalf("report = %+v, want nothing attempted on an exhausted budget", report)
	}
	if guardCalls != 0 {
		t.Errorf("cleanup guard ran %d times on an exhausted budget", guardCalls)
	}
	if !report.Deferred {
		t.Error("an exhausted budget did not report a deferral")
	}
	assertCleanupPendingRecords(t, m, wt.key, wt.RunID)
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("a deferred pass changed the worktree: %v", err)
	}
}

// TestClassifyCleanupWarningSeparatesRemediations is #5264's classification
// requirement: a file-holder refusal, a recovery-capacity refusal and a
// malformed durable record need three different operator actions, so they must
// not arrive as one undifferentiated warning list.
//
// The capacity case is the one worth pinning hardest. It arrives wrapped in
// ErrCleanupDeferred, so a classifier that tested the generic sentinel first
// would flatten it into "handoff deferred" — telling the operator to wait when
// the correct action is to free capacity.
func TestClassifyCleanupWarningSeparatesRemediations(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want CleanupWarningClass
	}{
		{"nil", nil, CleanupWarningUnknown},
		{
			"malformed record",
			fmt.Errorf("%w: %w", errCleanupRecordInvalid, errors.New("unexpected end of JSON input")),
			CleanupWarningRecord,
		},
		{
			"recovery capacity, wrapped in the generic deferral",
			fmt.Errorf("%w: %w: 8 of 8 slots used", ErrCleanupDeferred, ErrCleanupRecoveryCapacity),
			CleanupWarningRecoveryCapacity,
		},
		{
			"handoff still in progress",
			fmt.Errorf("%w: recovery handoff for wt-1: publishing", ErrCleanupDeferred),
			CleanupWarningHandoff,
		},
		{
			"file holder: busy",
			fmt.Errorf("remove worktree: %w", syscall.EBUSY),
			CleanupWarningFilesystem,
		},
		{
			"file holder: directory not empty",
			fmt.Errorf("remove worktree: %w", syscall.ENOTEMPTY),
			CleanupWarningFilesystem,
		},
		{
			"file holder: permission denied",
			fmt.Errorf("remove worktree: %w", fs.ErrPermission),
			CleanupWarningFilesystem,
		},
		{
			"unrecognized stays unclassified rather than mislabeled",
			errors.New("git worktree prune: exit status 128"),
			CleanupWarningUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCleanupWarning(tc.err); got != tc.want {
				t.Errorf("classifyCleanupWarning(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestCleanupRetryClassifiesMalformedRecordWarning wires the classification
// through a real pass, so the class reaches the report rather than only existing
// in the classifier.
func TestCleanupRetryClassifiesMalformedRecordWarning(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt := seedPendingPopulation(t, ctx, m, repo, []string{"run-broken"})[0]
	if err := os.WriteFile(m.markerPath(wt.key, wt.RunID), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 4})
	if err != nil {
		t.Fatalf("RetryCleanupPending: %v", err)
	}
	var found bool
	for _, warning := range report.Warnings {
		if warning.Class == CleanupWarningRecord {
			found = true
		}
	}
	if !found {
		t.Fatalf("no record-class warning for an unreadable marker: %+v", report.Warnings)
	}
}

func hasWarningContaining(warnings []ReapWarning, substr string) bool {
	for _, warning := range warnings {
		if warning.Err != nil && strings.Contains(warning.Err.Error(), substr) {
			return true
		}
	}
	return false
}

// keyForTestRepo returns the single repository key under the manager's root.
func keyForTestRepo(t *testing.T, m *Manager) string {
	t.Helper()
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return entry.Name()
		}
	}
	t.Fatal("no repository directory under the manager root")
	return ""
}
