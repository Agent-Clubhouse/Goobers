package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetryCleanupPendingConvergesFromDurableRecords(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return errors.New("recovery temporarily unavailable")
	}); err != nil {
		t.Fatal(err)
	}
	wt := createPendingRetryWorktree(t, ctx, m, repo, "owner-stage", "owner")
	assertCleanupPendingRecords(t, m, wt.key, wt.RunID)

	// The marker pair, not process memory, is the queue. A new manager can
	// therefore finish the cleanup after a daemon restart.
	restarted, err := NewManager(m.Root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := restarted.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 8})
	if err != nil {
		t.Fatalf("RetryCleanupPending: %v", err)
	}
	if report.Attempted != 1 || len(report.Removed) != 1 || len(report.Warnings) != 0 {
		t.Fatalf("retry report = %+v, want one successful attempt", report)
	}
	if report.Removed[0].Reason != ReapReasonCleanupPending {
		t.Fatalf("retry reason = %q, want %q", report.Removed[0].Reason, ReapReasonCleanupPending)
	}
	for _, path := range []string{
		wt.Path,
		m.markerPath(wt.key, wt.RunID),
		m.ownershipPath(wt.key, filepath.Base(wt.Path)),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("pending cleanup left %s: %v", path, err)
		}
	}
	registered, err := worktreeRegistered(ctx, m.repoDirForKey(wt.key), wt.Path)
	if err != nil || registered {
		t.Fatalf("pending cleanup registration = %v, %v; want false", registered, err)
	}
}

func TestRetryCleanupPendingRefusesDisagreeingOrMissingOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(*testing.T, *Manager, *Worktree)
	}{
		{
			name: "status disagreement",
			tamper: func(t *testing.T, m *Manager, wt *Worktree) {
				path := m.ownershipPath(wt.key, filepath.Base(wt.Path))
				mk, err := readMarker(path)
				if err != nil {
					t.Fatal(err)
				}
				mk.Status = statusActive
				if err := writeMarker(path, mk); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing ownership",
			tamper: func(t *testing.T, m *Manager, wt *Worktree) {
				if err := os.Remove(m.ownershipPath(wt.key, filepath.Base(wt.Path))); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newSourceRepo(t)
			m := newTestManager(t)
			if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
				return errors.New("defer")
			}); err != nil {
				t.Fatal(err)
			}
			wt := createPendingRetryWorktree(t, ctx, m, repo, "owner-stage", "owner")
			tamper := tc.tamper
			tamper(t, m, wt)

			report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if report.Attempted != 1 || len(report.Removed) != 0 || len(report.Warnings) != 1 {
				t.Fatalf("retry report = %+v, want one refused attempt", report)
			}
			if _, err := os.Stat(wt.Path); err != nil {
				t.Fatalf("refused retry changed worktree: %v", err)
			}
		})
	}
}

func TestRetryCleanupPendingRefusesNonCanonicalDurableIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(*marker, string)
	}{
		{name: "traversing owner slash", tamper: func(m *marker, _ string) { m.OwnerRunID = "../foreign" }},
		{name: "traversing owner backslash", tamper: func(m *marker, _ string) { m.OwnerRunID = `..\foreign` }},
		{name: "truncated repository digest", tamper: func(m *marker, key string) { m.RepositoryDigest = key }},
		{name: "malformed repository digest", tamper: func(m *marker, key string) { m.RepositoryDigest = key + strings.Repeat("z", 48) }},
		{name: "missing creation provenance", tamper: func(m *marker, _ string) { m.CreatedAt = time.Time{} }},
		{name: "missing process provenance", tamper: func(m *marker, _ string) { m.PID = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newSourceRepo(t)
			m := newTestManager(t)
			if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { return errors.New("defer") }); err != nil {
				t.Fatal(err)
			}
			wt := createPendingRetryWorktree(t, ctx, m, repo, "owner-stage", "owner")
			primaryPath := m.markerPath(wt.key, wt.RunID)
			primary, err := readMarker(primaryPath)
			if err != nil {
				t.Fatal(err)
			}
			tc.tamper(&primary, wt.key)
			ownershipPath := m.ownershipPath(wt.key, filepath.Base(wt.Path))
			if err := writeMarker(primaryPath, primary); err != nil {
				t.Fatal(err)
			}
			if err := writeMarker(ownershipPath, primary); err != nil {
				t.Fatal(err)
			}

			report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if report.Attempted != 1 || len(report.Removed) != 0 || len(report.Warnings) != 1 {
				t.Fatalf("retry report = %+v, want one refused attempt", report)
			}
			for _, path := range []string{wt.Path, primaryPath, ownershipPath} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("refused retry changed evidence %s: %v", path, err)
				}
			}
		})
	}
}

func TestReapRevalidatesScannedMarkerBeforeDeletingRecreatedWorktree(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { return errors.New("defer") }); err != nil {
		t.Fatal(err)
	}
	old := createPendingRetryWorktree(t, ctx, m, repo, "same-stage", "old-owner")
	markerPath := m.markerPath(old.key, old.RunID)
	scanned, err := readMarker(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 1}); err != nil || len(report.Removed) != 1 {
		t.Fatalf("retry old workspace = %+v, %v", report, err)
	}
	recreated, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: old.RunID, OwnerRunID: "new-owner", BaseRef: "main", Branch: "goobers/test/recreated",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reapOne(ctx, old.key, recreated.Path, markerPath, &scanned); !errors.Is(err, errReapAuthorityChanged) {
		t.Fatalf("stale reap error = %v, want changed-authority refusal", err)
	}
	if _, err := os.Stat(recreated.Path); err != nil {
		t.Fatalf("stale reap deleted recreated live worktree: %v", err)
	}
	current, err := readMarker(markerPath)
	if err != nil || current.OwnerRunID != "new-owner" || current.Status != statusActive {
		t.Fatalf("recreated marker = %+v, %v; want new active owner", current, err)
	}
}

func TestRetryCleanupPendingIgnoresActiveAndKeptWorktrees(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	active, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "active-stage", OwnerRunID: "active", BaseRef: "main", Branch: "goobers/test/active",
	})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "kept-stage", OwnerRunID: "kept", BaseRef: "main", Branch: "goobers/test/kept",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := kept.Remove(ctx, RemoveOptions{Keep: true}); err != nil {
		t.Fatal(err)
	}
	report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if report.Attempted != 0 || len(report.Removed) != 0 || len(report.Warnings) != 0 {
		t.Fatalf("retry report = %+v, want active and kept ignored", report)
	}
	for _, path := range []string{active.Path, kept.Path} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retry changed preserved worktree %s: %v", path, err)
		}
	}
}

func TestRetryCleanupPendingLimitAndCursorPreventStarvation(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	calls := map[string]int{}
	if err := m.SetCleanupGuard("recovery", func(_ context.Context, target CleanupTarget) error {
		calls[target.WorktreeID]++
		return errors.New("still blocked")
	}); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run-a", "run-b", "run-c"} {
		createPendingRetryWorktree(t, ctx, m, repo, runID, "owner")
	}
	clear(calls) // Ignore the initial Remove attempts.

	first, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.Attempted != 2 || len(first.Warnings) != 2 || first.Next == "" {
		t.Fatalf("first retry report = %+v", first)
	}
	second, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{After: first.Next, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if second.Attempted != 2 || len(second.Warnings) != 2 {
		t.Fatalf("second retry report = %+v", second)
	}
	if calls["run-c"] != 1 {
		t.Fatalf("later pending candidate calls = %d, want 1 after cursor rotation; all calls=%v", calls["run-c"], calls)
	}
	for runID, count := range calls {
		if count > 2 {
			t.Fatalf("candidate %s attempted %d times across two bounded passes", runID, count)
		}
	}
}

func TestRetryCleanupPendingCancellationPreservesEvidence(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return errors.New("defer")
	}); err != nil {
		t.Fatal(err)
	}
	wt := createPendingRetryWorktree(t, ctx, m, repo, "owner-stage", "owner")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.RetryCleanupPending(canceled, CleanupRetryOptions{Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RetryCleanupPending error = %v, want context cancellation", err)
	}
	assertCleanupPendingRecords(t, m, wt.key, wt.RunID)
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("canceled retry changed worktree: %v", err)
	}
}

func TestRetryCleanupPendingConcurrentPassesSerialize(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return errors.New("defer")
	}); err != nil {
		t.Fatal(err)
	}
	wt := createPendingRetryWorktree(t, ctx, m, repo, "owner-stage", "owner")
	started := make(chan struct{})
	release := make(chan struct{})
	if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		select {
		case <-started:
		default:
			close(started)
			<-release
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		report CleanupRetryReport
		err    error
	}
	results := make(chan outcome, 2)
	run := func() {
		report, err := m.RetryCleanupPending(ctx, CleanupRetryOptions{Limit: 1})
		results <- outcome{report: report, err: err}
	}
	go run()
	<-started
	go run()
	close(release)
	removed := 0
	for range 2 {
		result := <-results
		if result.err != nil || len(result.report.Warnings) != 0 {
			t.Fatalf("concurrent retry = %+v, %v", result.report, result.err)
		}
		removed += len(result.report.Removed)
	}
	if removed != 1 {
		t.Fatalf("concurrent retries removed %d worktrees, want exactly one", removed)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("serialized cleanup left worktree: %v", err)
	}
}

func createPendingRetryWorktree(t *testing.T, ctx context.Context, m *Manager, repo, runID, ownerRunID string) *Worktree {
	t.Helper()
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: runID, OwnerRunID: ownerRunID, BaseRef: "main", Branch: "goobers/test/" + runID,
	})
	if err != nil {
		t.Fatalf("Create %s: %v", runID, err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("Remove %s error = %v, want deferred cleanup", runID, err)
	}
	return wt
}
