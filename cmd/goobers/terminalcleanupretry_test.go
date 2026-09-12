package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/worktree"
)

func TestTerminalCleanupRetryStartsAfterReadinessAndRunsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager, wt := daemonPendingWorktree(t, context.Background())
	if err := manager.SetCleanupGuard("recovery", func(context.Context, worktree.CleanupTarget) error { return nil }); err != nil {
		t.Fatal(err)
	}
	setup := &schedulerSetup{WorktreesByGaggle: map[string]*worktree.Manager{"alpha": manager}}

	registry := newTerminalCleanupRetryRegistry(setup)
	notReadyDone := startTerminalCleanupRetry(ctx, registry, newSweepErrorReporter(nil, "test"), false)
	select {
	case <-notReadyDone:
	default:
		t.Fatal("retry runtime for a non-ready daemon did not stop immediately")
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("non-ready runtime changed pending worktree: %v", err)
	}

	done := startTerminalCleanupRetry(ctx, registry, newSweepErrorReporter(nil, "test"), true)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(wt.Path); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("immediate retry did not remove pending worktree")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry runtime did not join shutdown")
	}
}

func TestTerminalCleanupRetryCancellationPreservesPendingWorktree(t *testing.T) {
	manager, wt := daemonPendingWorktree(t, context.Background())
	started := make(chan struct{})
	if err := manager.SetCleanupGuard("recovery", func(ctx context.Context, _ worktree.CleanupTarget) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	registry := newTerminalCleanupRetryRegistry(&schedulerSetup{
		WorktreesByGaggle: map[string]*worktree.Manager{"alpha": manager},
	})
	done := startTerminalCleanupRetry(ctx, registry, newSweepErrorReporter(nil, "test"), true)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup retry did not reach blocking guard")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup retry did not cancel and join")
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("canceled cleanup removed pending worktree: %v", err)
	}
}

func TestTerminalCleanupRetryFollowsReplacedAndAddedManagers(t *testing.T) {
	oldInterval := terminalCleanupRetryInterval
	terminalCleanupRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { terminalCleanupRetryInterval = oldInterval })

	stale, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.SetCleanupGuard("recovery", func(context.Context, worktree.CleanupTarget) error { return errors.New("defer") }); err != nil {
		t.Fatal(err)
	}
	stalePending, err := stale.Create(context.Background(), worktree.CreateOptions{
		RepoURL: newDaemonFixtureRepo(t), RunID: "stale-stage", OwnerRunID: "stale", BaseRef: "main", Branch: "goobers/test/stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement, replacedPending := daemonPendingWorktree(t, context.Background())
	added, addedPending := daemonPendingWorktree(t, context.Background())
	for _, manager := range []*worktree.Manager{replacement, added} {
		if err := manager.SetCleanupGuard("recovery", func(context.Context, worktree.CleanupTarget) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	registry := newTerminalCleanupRetryRegistry(&schedulerSetup{
		WorktreesByGaggle: map[string]*worktree.Manager{"alpha": stale},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startTerminalCleanupRetry(ctx, registry, newSweepErrorReporter(nil, "test"), true)
	registry.Replace(map[string]*worktree.Manager{"alpha": replacement, "beta": added}, nil)
	// The old runner remains valid across Scheduler.Reload and can surrender a
	// workspace only after its manager has been displaced.
	if err := stalePending.Remove(context.Background(), worktree.RemoveOptions{}); !errors.Is(err, worktree.ErrCleanupDeferred) {
		t.Fatalf("late old-manager Remove error = %v, want deferred cleanup", err)
	}
	if err := stale.SetCleanupGuard("recovery", func(context.Context, worktree.CleanupTarget) error { return nil }); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for _, pending := range []*worktree.Worktree{stalePending, replacedPending, addedPending} {
		for {
			if _, err := os.Stat(pending.Path); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("reload replacement did not retry %s", pending.Path)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry runtime did not join after registry replacement")
	}
}

func TestTerminalCleanupRetryCursorsAreIndependentForSharedRootManagers(t *testing.T) {
	root := t.TempDir()
	first, err := worktree.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := worktree.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetCleanupGuard("recovery", func(context.Context, worktree.CleanupTarget) error { return errors.New("defer") }); err != nil {
		t.Fatal(err)
	}
	repo := newDaemonFixtureRepo(t)
	for i := 0; i <= terminalCleanupRetryBatch; i++ {
		runID := fmt.Sprintf("blocked-%02d", i)
		wt, err := first.Create(context.Background(), worktree.CreateOptions{
			RepoURL: repo, RunID: runID, OwnerRunID: "owner", BaseRef: "main", Branch: "goobers/test/" + runID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := wt.Remove(context.Background(), worktree.RemoveOptions{}); !errors.Is(err, worktree.ErrCleanupDeferred) {
			t.Fatalf("Remove %s = %v, want deferred cleanup", runID, err)
		}
	}
	attempts := map[string]int{}
	block := func(_ context.Context, target worktree.CleanupTarget) error {
		attempts[target.WorktreeID]++
		return errors.New("still blocked")
	}
	for _, manager := range []*worktree.Manager{first, second} {
		if err := manager.SetCleanupGuard("recovery", block); err != nil {
			t.Fatal(err)
		}
	}
	registry := newTerminalCleanupRetryRegistry(&schedulerSetup{WorktreesByGaggle: map[string]*worktree.Manager{
		"alpha": first, "beta": second,
	}})
	state := &terminalCleanupRetryState{}
	for range 3 {
		_ = state.run(context.Background(), registry.Snapshot(), terminalCleanupRetryBatch)
	}
	last := fmt.Sprintf("blocked-%02d", terminalCleanupRetryBatch)
	if attempts[last] == 0 {
		t.Fatalf("candidate after first batch was starved; attempts = %v", attempts)
	}
}

func TestTerminalCleanupRetryFinalCleansLatePendingWork(t *testing.T) {
	manager, pending := daemonPendingWorktree(t, context.Background())
	if err := manager.SetCleanupGuard("recovery", func(context.Context, worktree.CleanupTarget) error { return nil }); err != nil {
		t.Fatal(err)
	}
	registry := newTerminalCleanupRetryRegistry(&schedulerSetup{
		WorktreesByGaggle: map[string]*worktree.Manager{"alpha": manager},
	})
	runTerminalCleanupRetryFinal(registry, newSweepErrorReporter(nil, "test"), true)
	if _, err := os.Stat(pending.Path); !os.IsNotExist(err) {
		t.Fatalf("final retry left late pending worktree: %v", err)
	}
}

func daemonPendingWorktree(t *testing.T, ctx context.Context) (*worktree.Manager, *worktree.Worktree) {
	t.Helper()
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetCleanupGuard("recovery", func(context.Context, worktree.CleanupTarget) error {
		return errors.New("temporarily unavailable")
	}); err != nil {
		t.Fatal(err)
	}
	wt, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: newDaemonFixtureRepo(t), RunID: "run-stage", OwnerRunID: "run", BaseRef: "main", Branch: "goobers/test/run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(ctx, worktree.RemoveOptions{}); !errors.Is(err, worktree.ErrCleanupDeferred) {
		t.Fatalf("Remove error = %v, want deferred cleanup", err)
	}
	return manager, wt
}
