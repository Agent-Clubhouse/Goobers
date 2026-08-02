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

func pinnedFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	repo := newSourceRepo(t)
	mustWriteFile(t, filepath.Join(repo, ".gitignore"), "build/\n")
	runTestGit(t, repo, "add", ".gitignore")
	runTestGit(t, repo, "commit", "-m", "ignore build output")
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return manager, repo
}

func acquirePinnedFixture(t *testing.T, manager *Manager, repo, runID string, policy PinnedCleanPolicy) *PinnedLease {
	t.Helper()
	lease, err := manager.AcquirePinned(context.Background(), PinnedOptions{
		RepoURL: repo, RunID: runID, BaseRef: "main",
		Branch: "goobers/test/" + runID, CleanPolicy: policy,
	})
	if err != nil {
		t.Fatalf("AcquirePinned(%s): %v", runID, err)
	}
	return lease
}

func TestAcquirePinnedReusesWorkspaceAndPreservesBuildState(t *testing.T) {
	manager, repo := pinnedFixture(t)
	first := acquirePinnedFixture(t, manager, repo, "run-one", PinnedCleanNone)
	pinPath := first.Worktree.Path
	if origin := strings.TrimSpace(runTestGit(t, pinPath, "remote", "get-url", "origin")); origin != repo {
		t.Fatalf("pinned origin = %q, want push remote %q", origin, repo)
	}
	mustWriteFile(t, filepath.Join(pinPath, "build", "incremental.obj"), "warm")
	runTestGit(t, pinPath, "push", "origin", "HEAD")
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second := acquirePinnedFixture(t, manager, repo, "run-two", PinnedCleanNone)
	defer func() { _ = second.Release() }()
	if second.Worktree.Path != pinPath {
		t.Fatalf("second workspace = %q, want stable pin %q", second.Worktree.Path, pinPath)
	}
	if got, err := os.ReadFile(filepath.Join(pinPath, "build", "incremental.obj")); err != nil || string(got) != "warm" {
		t.Fatalf("preserved build state = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(manager.pinnedRoot, repoKey(repo), "runs")); !os.IsNotExist(err) {
		t.Fatalf("pinned mode created a per-run worktree directory: %v", err)
	}
}

func TestPinnedDiffUsesRefreshedBaseAfterConsecutiveRun(t *testing.T) {
	manager, repo := pinnedFixture(t)
	first := acquirePinnedFixture(t, manager, repo, "run-one", PinnedCleanNone)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(repo, "base-update.txt"), "latest")
	runTestGit(t, repo, "add", "base-update.txt")
	runTestGit(t, repo, "commit", "-m", "advance base")

	second := acquirePinnedFixture(t, manager, repo, "run-two", PinnedCleanNone)
	defer func() { _ = second.Release() }()
	diff, err := second.Worktree.Diff(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) != 0 {
		t.Fatalf("pinned diff includes refreshed base changes:\n%s", diff)
	}
}

func TestPreparePinnedSelectsRemoteBranchAndSyncsLatestBase(t *testing.T) {
	manager, repo := pinnedFixture(t)
	runTestGit(t, repo, "branch", "goobers/remediation/pr", "main")
	lease := acquirePinnedFixture(t, manager, repo, "run-one", PinnedCleanNone)
	defer func() { _ = lease.Release() }()

	if err := lease.Worktree.PreparePinned(context.Background(), PinnedPrepareOptions{
		BaseRef: "main", Branch: "goobers/remediation/pr", RequireExistingBranch: true,
	}); err != nil {
		t.Fatalf("prepare remote branch: %v", err)
	}
	if got := strings.TrimSpace(runTestGit(t, lease.Worktree.Path, "branch", "--show-current")); got != "goobers/remediation/pr" {
		t.Fatalf("prepared branch = %q, want goobers/remediation/pr", got)
	}

	mustWriteFile(t, filepath.Join(repo, "base-update.txt"), "latest")
	runTestGit(t, repo, "add", "base-update.txt")
	runTestGit(t, repo, "commit", "-m", "advance base")

	if err := lease.Worktree.PreparePinned(context.Background(), PinnedPrepareOptions{
		BaseRef: "main", Branch: "goobers/remediation/pr", RequireExistingBranch: true, SyncBase: true,
	}); err != nil {
		t.Fatalf("sync prepared branch: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(lease.Worktree.Path, "base-update.txt")); err != nil || string(got) != "latest" {
		t.Fatalf("synced base file = %q, %v", got, err)
	}
}

func TestAcquirePinnedCleanPolicies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy      PinnedCleanPolicy
		wantIgnored bool
	}{
		{name: "ignored-safe", policy: PinnedCleanIgnoredSafe, wantIgnored: true},
		{name: "full", policy: PinnedCleanFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repo := pinnedFixture(t)
			first := acquirePinnedFixture(t, manager, repo, "run-one", PinnedCleanNone)
			mustWriteFile(t, filepath.Join(first.Worktree.Path, "untracked.txt"), "remove")
			mustWriteFile(t, filepath.Join(first.Worktree.Path, "build", "cache.bin"), "cache")
			if err := first.Release(); err != nil {
				t.Fatal(err)
			}
			second := acquirePinnedFixture(t, manager, repo, "run-two", tc.policy)
			defer func() { _ = second.Release() }()
			if _, err := os.Stat(filepath.Join(second.Worktree.Path, "untracked.txt")); !os.IsNotExist(err) {
				t.Fatalf("untracked file survived %s: %v", tc.policy, err)
			}
			_, err := os.Stat(filepath.Join(second.Worktree.Path, "build", "cache.bin"))
			if tc.wantIgnored && err != nil {
				t.Fatalf("ignored build state did not survive %s: %v", tc.policy, err)
			}
			if !tc.wantIgnored && !os.IsNotExist(err) {
				t.Fatalf("ignored build state survived %s: %v", tc.policy, err)
			}
		})
	}
}

func TestAcquirePinnedSerializesRunsAndReportsQueuePosition(t *testing.T) {
	manager, repo := pinnedFixture(t)
	first := acquirePinnedFixture(t, manager, repo, "run-one", PinnedCleanNone)

	queued := make(chan int, 1)
	acquired := make(chan *PinnedLease, 1)
	errs := make(chan error, 1)
	go func() {
		lease, err := manager.AcquirePinned(context.Background(), PinnedOptions{
			RepoURL: repo, RunID: "run-two", BaseRef: "main", Branch: "goobers/test/run-two",
			OnQueuePosition: func(position int) error {
				select {
				case queued <- position:
				default:
				}
				return nil
			},
		})
		if err != nil {
			errs <- err
			return
		}
		acquired <- lease
	}()

	select {
	case position := <-queued:
		if position != 2 {
			t.Fatalf("queue position = %d, want 2", position)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second run did not report queue position")
	}
	select {
	case <-acquired:
		t.Fatal("second run acquired the workspace before the first released it")
	case err := <-errs:
		t.Fatalf("second run failed while queued: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-acquired:
		if err := second.Release(); err != nil {
			t.Fatal(err)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("second run did not acquire after release")
	}
}

func TestAcquirePinnedRefusesStaleLease(t *testing.T) {
	manager, repo := pinnedFixture(t)
	first := acquirePinnedFixture(t, manager, repo, "crashed-run", PinnedCleanNone)
	if err := first.handle.Release(); err != nil {
		t.Fatal(err)
	}

	_, err := manager.AcquirePinned(context.Background(), PinnedOptions{
		RepoURL: repo, RunID: "next-run", BaseRef: "main", Branch: "goobers/test/next-run",
	})
	var stale *StalePinnedLeaseError
	if !errors.As(err, &stale) || stale.RunID != "crashed-run" {
		t.Fatalf("AcquirePinned error = %v, want stale lease for crashed-run", err)
	}
}

func TestPinnedWorkspaceIsOutsideRetentionInventory(t *testing.T) {
	manager, repo := pinnedFixture(t)
	lease := acquirePinnedFixture(t, manager, repo, "retention-run", PinnedCleanNone)
	pinPath := lease.Worktree.Path
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	results, warnings, err := PruneRetained(context.Background(), []*Manager{manager}, RetentionOptions{
		Delete: true, MaxAge: time.Nanosecond, MaxRetainedBytes: 1, Now: time.Now().Add(time.Hour),
	})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("PruneRetained: warnings=%v err=%v", warnings, err)
	}
	for _, result := range results {
		if result.Path == pinPath {
			t.Fatalf("pinned workspace entered retention inventory: %+v", result)
		}
	}
	if _, err := os.Stat(pinPath); err != nil {
		t.Fatalf("retention removed pinned workspace: %v", err)
	}
}
