package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPruneRetainedDryRunOrdersWindowCandidatesAndExcludesNonterminal(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	manager := newTestManager(t)
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)

	oldest := retainedFixture(t, manager, repo, "oldest-stage", "failed-oldest", now.Add(-4*time.Hour))
	tieA := retainedFixture(t, manager, repo, "tie-a-stage", "failed-a", now.Add(-3*time.Hour))
	tieZ := retainedFixture(t, manager, repo, "tie-z-stage", "failed-z", now.Add(-3*time.Hour))
	running := retainedFixture(t, manager, repo, "running-stage", "running", now.Add(-24*time.Hour))
	completed := retainedFixture(t, manager, repo, "completed-stage", "completed", now.Add(-24*time.Hour))

	results, warnings, err := PruneRetained(ctx, []*Manager{manager}, RetentionOptions{
		MaxAge: time.Hour,
		Now:    now,
		IsTerminalFailure: func(_, _, owner string) (bool, error) {
			return owner != "running" && owner != "completed", nil
		},
	})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("PruneRetained = warnings %+v, err %v", warnings, err)
	}
	want := []string{oldest.RunID, tieA.RunID, tieZ.RunID}
	if len(results) != len(want) {
		t.Fatalf("results = %+v, want %d candidates", results, len(want))
	}
	for i, result := range results {
		if result.WorktreeID != want[i] || result.Rule != RetentionRuleWindow || !result.DryRun || result.Deleted || result.BytesReclaimed != 0 {
			t.Fatalf("result[%d] = %+v, want dry-run window candidate %s", i, result, want[i])
		}
	}
	for _, wt := range []*Worktree{oldest, tieA, tieZ, running, completed} {
		if _, err := os.Stat(wt.Path); err != nil {
			t.Fatalf("dry-run removed %s: %v", wt.RunID, err)
		}
	}
}

func TestPruneRetainedAppliesStorageCapAcrossManagers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	firstManager := newTestManager(t)
	secondManager := newTestManager(t)
	first := retainedFixture(t, firstManager, newSourceRepo(t), "first-stage", "failed-first", now.Add(-2*time.Hour))
	second := retainedFixture(t, secondManager, newSourceRepo(t), "second-stage", "failed-second", now.Add(-time.Hour))
	firstBytes := mustDirectorySize(t, first.Path)
	secondBytes := mustDirectorySize(t, second.Path)

	results, warnings, err := PruneRetained(ctx, []*Manager{secondManager, firstManager}, RetentionOptions{
		MaxRetainedBytes: secondBytes,
		Now:              now,
		IsTerminalFailure: func(_, _, _ string) (bool, error) {
			return true, nil
		},
	})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("PruneRetained = warnings %+v, err %v", warnings, err)
	}
	if len(results) != 1 || results[0].WorktreeID != first.RunID || results[0].Rule != RetentionRuleStorageCap {
		t.Fatalf("results = %+v, want oldest cross-manager storage candidate", results)
	}
	if results[0].Bytes != firstBytes {
		t.Fatalf("candidate bytes = %d, want %d", results[0].Bytes, firstBytes)
	}
}

func TestPruneRetainedSerializesConcurrentDeletionAndCountsOnlySuccess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t)
	wt := retainedFixture(t, manager, newSourceRepo(t), "concurrent-stage", "failed", now.Add(-2*time.Hour))
	wantBytes := mustDirectorySize(t, wt.Path)
	opts := RetentionOptions{
		Delete: true,
		MaxAge: time.Hour,
		Now:    now,
		IsTerminalFailure: func(_, _, _ string) (bool, error) {
			return true, nil
		},
	}

	start := make(chan struct{})
	allResults := make(chan []RetentionResult, 2)
	allErrors := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results, warnings, err := PruneRetained(ctx, []*Manager{manager}, opts)
			if len(warnings) != 0 {
				err = errorsJoin(err, warnings[0].Err)
			}
			allResults <- results
			allErrors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(allResults)
	close(allErrors)

	for err := range allErrors {
		if err != nil {
			t.Fatalf("concurrent PruneRetained: %v", err)
		}
	}
	var deleted, reclaimed int64
	var candidates int
	for results := range allResults {
		candidates += len(results)
		for _, result := range results {
			if result.Err != nil {
				t.Fatalf("deletion result error: %v", result.Err)
			}
			if result.Deleted {
				deleted++
				reclaimed += result.BytesReclaimed
			}
		}
	}
	if candidates != 1 || deleted != 1 || reclaimed != wantBytes {
		t.Fatalf("concurrent totals: candidates=%d deleted=%d reclaimed=%d; want 1, 1, %d", candidates, deleted, reclaimed, wantBytes)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("retained worktree still exists: %v", err)
	}
}

func TestPruneRetainedDeletesOnlyMergedTerminalRunBranches(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	manager := newTestManager(t)
	repoDir, err := manager.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy: %v", err)
	}

	merged, _ := committedRunBranch(t, manager, repo, "merged", "main")
	if err := merged.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("remove merged worktree: %v", err)
	}

	unmerged, _ := committedRunBranch(t, manager, repo, "unmerged", "goobers/workflow/merged")
	if err := unmerged.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("remove unmerged worktree: %v", err)
	}
	active, activeTip := committedRunBranch(t, manager, repo, "active", "goobers/workflow/merged")
	runTestGit(t, repoDir, "update-ref", "refs/heads/main", activeTip)

	results, warnings, err := PruneRetained(ctx, []*Manager{manager}, RetentionOptions{
		Delete: true,
		IsRunTerminal: func(_, runID string) (bool, error) {
			return runID != "active", nil
		},
	})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("PruneRetained = warnings %+v, err %v", warnings, err)
	}
	if len(results) != 1 || results[0].Branch != "goobers/workflow/merged" || !results[0].Deleted || results[0].Rule != RetentionRuleMergedBranch {
		t.Fatalf("branch results = %+v", results)
	}
	if branchExists(ctx, repoDir, "goobers/workflow/merged") {
		t.Fatal("merged terminal branch still exists")
	}
	if !branchExists(ctx, repoDir, "goobers/workflow/unmerged") {
		t.Fatal("unmerged terminal branch was deleted")
	}
	if !branchExists(ctx, repoDir, "goobers/workflow/active") {
		t.Fatal("active merged branch was deleted")
	}
	if err := active.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("remove active fixture: %v", err)
	}
}

func TestPruneRetainedReportsBranchDeletionFailureWithoutReclamation(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	manager := newTestManager(t)
	repoDir, err := manager.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy: %v", err)
	}
	wt, tip := committedRunBranch(t, manager, repo, "checked-out", "main")
	if err := wt.Remove(ctx, RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("keep worktree: %v", err)
	}
	runTestGit(t, repoDir, "update-ref", "refs/heads/main", tip)

	results, warnings, err := PruneRetained(ctx, []*Manager{manager}, RetentionOptions{
		Delete: true,
		IsTerminalFailure: func(_, _, _ string) (bool, error) {
			return true, nil
		},
		IsRunTerminal: func(_, _ string) (bool, error) {
			return true, nil
		},
	})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("PruneRetained = warnings %+v, err %v", warnings, err)
	}
	if len(results) != 1 || results[0].Kind != RetentionKindBranch || results[0].Err == nil || results[0].Deleted || results[0].BytesReclaimed != 0 {
		t.Fatalf("failed branch result = %+v", results)
	}
	if !branchExists(ctx, repoDir, "goobers/workflow/checked-out") {
		t.Fatal("failed deletion removed checked-out branch")
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("failed deletion removed retained worktree: %v", err)
	}
}

func retainedFixture(t *testing.T, manager *Manager, repo, worktreeID, ownerRunID string, retainedAt time.Time) *Worktree {
	t.Helper()
	wt, err := manager.Create(context.Background(), CreateOptions{
		RepoURL: repo, RunID: worktreeID, OwnerRunID: ownerRunID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create retained fixture: %v", err)
	}
	mustWriteFile(t, filepath.Join(wt.Path, "retained.data"), worktreeID)
	if err := wt.Remove(context.Background(), RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("keep retained fixture: %v", err)
	}
	mk, err := readMarker(manager.markerPath(wt.key, wt.RunID))
	if err != nil {
		t.Fatalf("read retained marker: %v", err)
	}
	mk.RetainedAt = retainedAt
	if err := writeMarker(manager.markerPath(wt.key, wt.RunID), mk); err != nil {
		t.Fatalf("backdate retained marker: %v", err)
	}
	return wt
}

func committedRunBranch(t *testing.T, manager *Manager, repo, runID, baseRef string) (*Worktree, string) {
	t.Helper()
	wt, err := manager.Create(context.Background(), CreateOptions{
		RepoURL: repo, RunID: runID + "-stage", OwnerRunID: runID, BaseRef: baseRef,
		Branch: "goobers/workflow/" + runID,
	})
	if err != nil {
		t.Fatalf("Create run branch %s: %v", runID, err)
	}
	mustWriteFile(t, filepath.Join(wt.Path, runID+".txt"), runID)
	runTestGit(t, wt.Path, "add", runID+".txt")
	runTestGit(t, wt.Path, "commit", "-m", runID)
	tip := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "HEAD"))
	return wt, tip
}

func mustDirectorySize(t *testing.T, path string) int64 {
	t.Helper()
	size, err := directorySize(path)
	if err != nil {
		t.Fatalf("directorySize: %v", err)
	}
	return size
}

func errorsJoin(first, second error) error {
	if first != nil {
		return first
	}
	return second
}
