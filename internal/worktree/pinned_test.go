package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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
	if !first.Worktree.PinnedWorkspaceCreated {
		t.Fatal("first AcquirePinned did not report workspace creation")
	}
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
	if second.Worktree.PinnedWorkspaceCreated {
		t.Fatal("second AcquirePinned reported workspace recreation")
	}
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

func TestResetPinnedKillsLockHoldersAndRematerializes(t *testing.T) {
	_, repo := pinnedFixture(t)
	var killedPath string
	var lockHolderExited bool
	var lockHolder *exec.Cmd
	manager, err := NewManager(t.TempDir(), WithPinnedProcessKiller(func(path string) error {
		killedPath = path
		if lockHolder == nil {
			return errors.New("lock holder was not started")
		}
		if err := lockHolder.Process.Kill(); err != nil {
			return err
		}
		if err := lockHolder.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return err
			}
		}
		lockHolderExited = true
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	lease := acquirePinnedFixture(t, manager, repo, "run-before-reset", PinnedCleanNone)
	pinPath := lease.Worktree.Path
	lockedPath := filepath.Join(pinPath, "build", "locked.obj")
	mustWriteFile(t, lockedPath, "poisoned")
	if _, err := lease.RecordOutcome(true); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	readyPath := filepath.Join(t.TempDir(), "lock-holder-ready")
	lockHolder = exec.Command(os.Args[0], "-test.run=TestPinnedWorkspaceLockHolderProcess")
	lockHolder.Env = append(os.Environ(),
		"GOOBERS_PINNED_LOCK_HOLDER="+lockedPath,
		"GOOBERS_PINNED_LOCK_READY="+readyPath,
	)
	if err := lockHolder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	t.Cleanup(func() {
		if lockHolder.ProcessState == nil {
			_ = lockHolder.Process.Kill()
			_ = lockHolder.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatalf("check lock-holder readiness: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("lock holder did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	root := filepath.Join(manager.pinnedRoot, repoKey(repo))
	stale, err := json.Marshal(pinnedLeaseRecord{RunID: "dead-run", PID: 999999})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pin.lease.json"), stale, 0o644); err != nil {
		t.Fatal(err)
	}

	resetPath, err := manager.ResetPinned(context.Background(), PinnedResetOptions{
		RepoURL: repo,
		BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if killedPath != pinPath {
		t.Fatalf("process killer path = %q, want %q", killedPath, pinPath)
	}
	if !lockHolderExited {
		t.Fatal("reset removed workspace before lock-holding process exited")
	}
	if resetPath != pinPath {
		t.Fatalf("reset path = %q, want stable path %q", resetPath, pinPath)
	}
	if _, err := os.Stat(filepath.Join(pinPath, "build", "locked.obj")); !os.IsNotExist(err) {
		t.Fatalf("poisoned build output survived reset: %v", err)
	}
	for _, name := range []string{"pin.lease.json", "failure-streak.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived reset: %v", name, err)
		}
	}
}

func TestPinnedWorkspaceLockHolderProcess(t *testing.T) {
	lockPath := os.Getenv("GOOBERS_PINNED_LOCK_HOLDER")
	if lockPath == "" {
		return
	}
	lock, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := os.WriteFile(os.Getenv("GOOBERS_PINNED_LOCK_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Minute)
}

func TestPinnedFailureStreakCountsOnlyConsecutiveFailures(t *testing.T) {
	manager, repo := pinnedFixture(t)
	lease := acquirePinnedFixture(t, manager, repo, "run-streak", PinnedCleanNone)
	defer func() { _ = lease.Release() }()

	for _, step := range []struct {
		failed bool
		want   int
	}{
		{failed: true, want: 1},
		{failed: true, want: 2},
		{failed: false, want: 0},
		{failed: true, want: 1},
	} {
		got, err := lease.RecordOutcome(step.failed)
		if err != nil {
			t.Fatal(err)
		}
		if got != step.want {
			t.Fatalf("RecordOutcome(%v) = %d, want %d", step.failed, got, step.want)
		}
	}
}

func TestResetPinnedRefusesLiveLease(t *testing.T) {
	manager, repo := pinnedFixture(t)
	lease := acquirePinnedFixture(t, manager, repo, "run-live", PinnedCleanNone)
	defer func() { _ = lease.Release() }()

	if _, err := manager.ResetPinned(context.Background(), PinnedResetOptions{
		RepoURL: repo,
		BaseRef: "main",
	}); err == nil || !strings.Contains(err.Error(), "leased by a live run") {
		t.Fatalf("ResetPinned error = %v, want live-lease refusal", err)
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

func TestAcquirePinnedResetsTrackedResidueBeforeAdvancedBaseCheckout(t *testing.T) {
	manager, repo := pinnedFixture(t)
	first := acquirePinnedFixture(t, manager, repo, "run-one", PinnedCleanNone)
	mustWriteFile(t, filepath.Join(first.Worktree.Path, "README.md"), "dirty prior run")
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(repo, "README.md"), "advanced base")
	runTestGit(t, repo, "add", "README.md")
	runTestGit(t, repo, "commit", "-m", "advance tracked base file")

	second := acquirePinnedFixture(t, manager, repo, "run-two", PinnedCleanNone)
	defer func() { _ = second.Release() }()
	got, err := os.ReadFile(filepath.Join(second.Worktree.Path, "README.md"))
	if err != nil || string(got) != "advanced base" {
		t.Fatalf("tracked file after reset = %q, %v", got, err)
	}
}

func TestPreparePinnedSelectsRemoteBranchAndSyncsLatestBase(t *testing.T) {
	_, repo := pinnedFixture(t)
	manager, err := NewManager(t.TempDir(), WithPinnedRoot(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if manager.Root == manager.pinnedRoot {
		t.Fatal("test requires distinct disposable and pinned roots")
	}
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

func TestAcquirePinnedRecoversOrphanedWaiterBeforeReportingStaleLease(t *testing.T) {
	manager, repo := pinnedFixture(t)
	first := acquirePinnedFixture(t, manager, repo, "crashed-holder", PinnedCleanNone)
	if err := first.handle.Release(); err != nil {
		t.Fatal(err)
	}

	const deadPID = 999999
	previousAlive := pinnedQueueProcessAlive
	pinnedQueueProcessAlive = func(pid int) bool { return pid != deadPID }
	t.Cleanup(func() { pinnedQueueProcessAlive = previousAlive })

	queueDir := filepath.Join(manager.pinnedRoot, repoKey(repo), "lease.queue")
	orphanPath := filepath.Join(queueDir, "00000000000000000000-999999-crashed-waiter")
	orphanRecord, err := json.Marshal(pinnedQueueRecord{RunID: "crashed-waiter", PID: deadPID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, orphanRecord, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = manager.AcquirePinned(context.Background(), PinnedOptions{
		RepoURL: repo, RunID: "next-run", BaseRef: "main", Branch: "goobers/test/next-run",
	})
	var stale *StalePinnedLeaseError
	if !errors.As(err, &stale) || stale.RunID != "crashed-holder" {
		t.Fatalf("AcquirePinned error = %v, want stale lease for crashed-holder", err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphaned queue entry was not removed: %v", err)
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

func TestAcquirePinnedAppliesPathLengthPreflightBeforeCheckout(t *testing.T) {
	repo := newSourceRepo(t)
	// deepest is the on-disk path used to create the fixture file; trackedPath
	// is the same path in git's own tracked-path form. `git ls-tree` (which the
	// preflight shells out to) always reports paths with forward slashes,
	// regardless of OS, so the preflight's error names trackedPath -- not
	// deepest, which is OS-native (backslashes on Windows) and only happens to
	// equal trackedPath on POSIX.
	deepest := filepath.Join("generated", strings.Repeat("x", 40), "header.hpp")
	trackedPath := strings.Join([]string{"generated", strings.Repeat("x", 40), "header.hpp"}, "/")
	mustWriteFile(t, filepath.Join(repo, deepest), "content")
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "add deep path")

	root := t.TempDir()
	checkoutPath := filepath.Join(root, repoKey(repo), "pin")
	available := len(trackedPath) - 1
	manager, err := NewManager(root, WithPathLengthLimit(repo, PathLengthLimit{
		MaxPathLength: len(checkoutPath) + 1 + available,
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.AcquirePinned(context.Background(), PinnedOptions{
		RepoURL: repo, RunID: "path-budget", BaseRef: "main",
	})
	if err == nil {
		t.Fatal("AcquirePinned succeeded despite exhausted path budget")
	}
	if !strings.Contains(err.Error(), trackedPath) {
		t.Fatalf("AcquirePinned error %q does not name deepest path %q", err, trackedPath)
	}
	if _, statErr := os.Stat(checkoutPath); !os.IsNotExist(statErr) {
		t.Fatalf("pinned checkout exists after preflight refusal: %v", statErr)
	}
}
