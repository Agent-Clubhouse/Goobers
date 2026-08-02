package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPinnedWorkspacePersistsBuildStateAndLivesOutsideRuns(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	manager := newTestManager(t)

	release, err := manager.AcquirePinnedLease(ctx, repo, "run-one", nil)
	if err != nil {
		t.Fatalf("AcquirePinnedLease: %v", err)
	}
	first, err := manager.PreparePinned(ctx, repo, "run-one", "main", "goobers/workflow/run-one", false, CleanNone)
	if err != nil {
		t.Fatalf("PreparePinned first run: %v", err)
	}
	buildState := filepath.Join(first.Path, "obj", "warm.cache")
	if err := os.MkdirAll(filepath.Dir(buildState), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(buildState, []byte("warm"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatalf("release first lease: %v", err)
	}

	release, err = manager.AcquirePinnedLease(ctx, repo, "run-two", nil)
	if err != nil {
		t.Fatalf("AcquirePinnedLease second run: %v", err)
	}
	defer func() { _ = release() }()
	second, err := manager.PreparePinned(ctx, repo, "run-two", "main", "goobers/workflow/run-two", false, CleanNone)
	if err != nil {
		t.Fatalf("PreparePinned second run: %v", err)
	}
	if first.Path != second.Path {
		t.Fatalf("pinned paths differ: %q and %q", first.Path, second.Path)
	}
	if filepath.Base(first.Path) != "pin" {
		t.Fatalf("pinned path = %q, want stable pin directory", first.Path)
	}
	if _, err := os.Stat(buildState); err != nil {
		t.Fatalf("none clean policy discarded build state: %v", err)
	}
	if _, err := os.Stat(manager.runsDirForKey(repoKey(repo))); !os.IsNotExist(err) {
		t.Fatalf("pinned provisioning created per-run worktree namespace: %v", err)
	}
	results, warnings, err := PruneRetained(ctx, []*Manager{manager}, RetentionOptions{
		Delete: true, MaxAge: time.Nanosecond, Now: time.Now().Add(time.Hour),
		IsTerminalFailure: func(_, _, _ string) (bool, error) { return true, nil },
	})
	if err != nil || len(warnings) != 0 || len(results) != 0 {
		t.Fatalf("retention considered pinned workspace: results=%+v warnings=%+v err=%v", results, warnings, err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatalf("retention removed pinned workspace: %v", err)
	}
}

func TestPinnedLeaseQueuesRunsFIFO(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t)
	repo := newSourceRepo(t)
	firstRelease, err := manager.AcquirePinnedLease(ctx, repo, "first", nil)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}

	position := make(chan int, 1)
	acquired := make(chan func() error, 1)
	go func() {
		release, acquireErr := manager.AcquirePinnedLease(ctx, repo, "second", func(p int) {
			position <- p
		})
		if acquireErr == nil {
			acquired <- release
		}
	}()
	select {
	case got := <-position:
		if got != 1 {
			t.Errorf("queue position = %d, want 1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("queued run did not report its position")
	}
	select {
	case <-acquired:
		t.Fatal("second run acquired workspace before first released it")
	default:
	}
	if err := firstRelease(); err != nil {
		t.Fatalf("release first lease: %v", err)
	}
	select {
	case release := <-acquired:
		if err := release(); err != nil {
			t.Fatalf("release second lease: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued run did not acquire workspace")
	}
}
