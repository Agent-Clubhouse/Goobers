package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// TestUpAPIReadyBeforeRetentionSweepCompletes is #4373's regression test:
// the daemon must bind its HTTP API (and reach the "daemon started"
// readiness line) without waiting for a broad worktree/branch retention
// sweep to finish, even when that sweep has real work to do and that work
// is slow. It uses worktree.DeleteBranchHook — a test-only seam on the
// exact git subprocess call site a large merged-branch backlog blocks on
// (166 branches took ~11 minutes on MDB5) — to make one merged-branch
// deletion hang indefinitely, then asserts readiness is still reached
// promptly. On the pre-fix synchronous call site, this test deadlocks and
// times out: readiness could never be reached until the (permanently
// blocked) deletion returned.
func TestUpAPIReadyBeforeRetentionSweepCompletes(t *testing.T) {
	root := initDeterministicDemo(t)

	instanceYAMLPath := filepath.Join(root, "instance.yaml")
	raw, err := os.ReadFile(instanceYAMLPath)
	if err != nil {
		t.Fatalf("read instance.yaml: %v", err)
	}
	updated := strings.Replace(string(raw), "retention: {}", "retention:\n  enabled: true\n  maxRetainedWorktreeBytes: 1\n", 1)
	if updated == string(raw) {
		t.Fatalf("instance.yaml scaffold did not contain expected retention: {} placeholder:\n%s", raw)
	}
	if err := os.WriteFile(instanceYAMLPath, []byte(updated), 0o644); err != nil {
		t.Fatalf("enable retention: %v", err)
	}

	layout := instance.NewLayout(root)
	gaggleLayout := layout.ForGaggle("example")
	manager, err := worktree.NewManager(gaggleLayout.WorkcopiesDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	repo, err := repoCloneURL(apiv1.RepoRef{})
	if err != nil {
		t.Fatalf("repoCloneURL: %v", err)
	}
	ctx := context.Background()
	const runID = "slow-sweep-owner"
	branch := providers.BranchName("implementation", runID)
	wt, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: repo, RunID: runID + "-stage", OwnerRunID: runID, BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatalf("create merged-branch fixture: %v", err)
	}
	if err := wt.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatalf("remove branch fixture worktree: %v", err)
	}
	createTerminalRun(t, gaggleLayout, runID)

	deleteStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	prevHook := worktree.DeleteBranchHook
	worktree.DeleteBranchHook = func() {
		select {
		case deleteStarted <- struct{}{}:
		default:
		}
		<-release
	}
	t.Cleanup(func() { worktree.DeleteBranchHook = prevHook })

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer

	done := make(chan int, 1)
	go func() { done <- runUpContext(runCtx, []string{root}, &stdout, &stderr) }()
	var waitOnce sync.Once
	var exitCode int
	var exitOK bool
	waitForExit := func() (int, bool) {
		waitOnce.Do(func() {
			closeRelease()
			cancel()
			select {
			case exitCode = <-done:
				exitOK = true
			case <-time.After(10 * time.Second):
			}
		})
		return exitCode, exitOK
	}
	t.Cleanup(func() {
		if _, ok := waitForExit(); !ok {
			t.Error("runUpContext did not return during cleanup")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stdout.String(), "daemon started") {
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not report readiness within 10s (retention sweep must not block API bind, #4373); stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Confirm the deferred sweep actually reached the slow deletion — not
	// skipped for an unrelated config reason — proving readiness above was
	// reached WHILE a real, still-in-flight deletion was blocked, not
	// because there was nothing to sweep.
	select {
	case <-deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("retention sweep never reached the merged-branch delete call after readiness")
	}

	code, ok := waitForExit()
	if !ok {
		t.Fatal("runUpContext did not return after ctx cancellation")
	}
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}
