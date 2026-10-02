//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// Exercise the real daemon startup wiring, not a standalone Reap call: moving
// pending cleanup ahead of readiness must fail even if Reap's unit tests pass.
func TestIntegrationStartupWithLargeTerminalCleanupBacklog(t *testing.T) {
	const population = 1009
	layout := instance.NewLayout(initDeterministicDemo(t))
	scoped := layout.ForGaggle("example")
	const owner = "terminal-backlog-owner"
	run, err := journal.Create(scoped.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: owner, Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example", StartedAt: time.Now().Add(-time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	// This run already finished its other finalization. Its surrendered
	// workspaces are queued by their own pending markers, not active-run work.
	if err := journal.ClearRunActive(filepath.Join(scoped.RunsDir(), owner)); err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(scoped.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetCleanupGuard("recovery", func(context.Context, worktree.CleanupTarget) error { return errors.New("handoff refused") }); err != nil {
		t.Fatal(err)
	}
	// Deliberately distinct from the configured repository. The daemon's own
	// recovery guard therefore refuses this population until an operator repairs
	// the handoff; readiness must not depend on that repair.
	source := newDaemonFixtureRepo(t)
	wt, err := manager.Create(t.Context(), worktree.CreateOptions{RepoURL: source, RunID: "pending-0000", OwnerRunID: owner, Gaggle: "example", BaseRef: "main", Branch: "goobers/test/pending"})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(t.Context(), worktree.RemoveOptions{}); !errors.Is(err, worktree.ErrCleanupDeferred) {
		t.Fatalf("seed refusal: %v", err)
	}
	repoRoot := filepath.Dir(filepath.Dir(wt.Path))
	seedTerminalBacklogRecords(t, repoRoot, owner, population)
	count, err := measureWorktreeAccumulation(map[string]*worktree.Manager{"example": manager}, nil)
	if err != nil || count != population {
		t.Fatalf("measured population=%d err=%v", count, err)
	}

	// The deadline grows with the real measured backlog, using the same budget
	// function as startup. There is no machine-speed-dependent seconds cutoff.
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	floor, err := cfg.Runner.LivenessTimeoutDuration()
	if err != nil {
		t.Fatal(err)
	}
	budget := deriveStartupBudget(floor, startupAccumulation{Worktrees: count})
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	stdout := newDaemonOutput()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{"--quiet", layout.Root}, stdout, &stderr) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	select {
	case <-stdout.started:
	case code := <-done:
		joined = true
		t.Fatalf("startup exited %d: %s\n%s", code, stderr.String(), stdout.String())
	case <-ctx.Done():
		t.Fatalf("did not reach readiness within derived budget %s: %s", budget, stdout.String())
	}
	startup := strings.SplitN(stdout.String(), "daemon started", 2)[0]
	if !strings.Contains(startup, "startup phase=ready status=done") {
		t.Fatal("daemon announced startup before readiness")
	}
	if !strings.Contains(startup, fmt.Sprintf("startup budget: worktrees=%d", population)) {
		t.Fatalf("startup did not measure backlog: %s", startup)
	}
	if strings.Contains(startup, "warning: skipped worktree cleanup") {
		t.Fatal("startup attempted refused pending handoffs before readiness")
	}
	if strings.Contains(startup, "state=exceeded") || strings.Contains(startup, "daemon alive but not ready after derived budget") {
		t.Fatalf("startup exceeded derived budget: %s", startup)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("shutdown=%d: %s", code, stderr.String())
	}
	joined = true
	if count, err := worktree.CountReapCandidates(manager.Root); err != nil || count != population {
		t.Fatalf("refused custody lost entries: count=%d err=%v", count, err)
	}

	// Drive the daemon's bounded post-readiness pass without waiting a minute
	// between passes. A persistently refused prefix must not starve the tail.
	attempts := make(map[string]int, population)
	var calls int
	release := false
	if err := manager.SetCleanupGuard("recovery", func(_ context.Context, target worktree.CleanupTarget) error {
		calls++
		attempts[target.WorktreeID]++
		if !release && target.WorktreeID == "pending-0000" {
			return errors.New("still refusing custody")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := &terminalCleanupRetryState{}
	targets := newTerminalCleanupRetryRegistry(&schedulerSetup{WorktreesByGaggle: map[string]*worktree.Manager{"example": manager}}).Snapshot()
	for pass := 0; pass < (population+terminalCleanupRetryBatch-1)/terminalCleanupRetryBatch+2; pass++ {
		before := calls
		_ = state.run(t.Context(), targets, terminalCleanupRetryBatch)
		if calls-before > terminalCleanupRetryBatch {
			t.Fatalf("pass %d attempted %d, limit %d", pass, calls-before, terminalCleanupRetryBatch)
		}
	}
	for i := 0; i < population; i++ {
		if attempts[fmt.Sprintf("pending-%04d", i)] == 0 {
			t.Fatalf("entry %d permanently unattempted", i)
		}
	}
	if count, err := worktree.CountReapCandidates(manager.Root); err != nil || count != 1 {
		t.Fatalf("eligible population did not drain: count=%d err=%v", count, err)
	}
	release = true
	if err := state.run(t.Context(), targets, terminalCleanupRetryBatch); err != nil {
		t.Fatal(err)
	}
	if count, err := worktree.CountReapCandidates(manager.Root); err != nil || count != 0 {
		t.Fatalf("released final entry did not drain: count=%d err=%v", count, err)
	}
	for _, name := range []string{"markers", "owners", "runs"} {
		entries, err := os.ReadDir(filepath.Join(repoRoot, name))
		if err != nil || len(entries) != 0 {
			t.Fatalf("durable %s records survived drain: count=%d err=%v", name, len(entries), err)
		}
	}
}

// Expand one Create/Remove-produced record pair into durable abandoned
// workspaces. No thousand Git clones are needed: unregistered directories with
// matching modern ownership records are a supported crash-cleanup population.
func seedTerminalBacklogRecords(t *testing.T, repoRoot, owner string, population int) {
	t.Helper()
	template, err := os.ReadFile(filepath.Join(repoRoot, "markers", "pending-0000.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if err := json.Unmarshal(template, &marker); err != nil {
		t.Fatal(err)
	}
	if marker["status"] != "cleanup-pending" || marker["owner_run_id"] != owner {
		t.Fatalf("invalid template: %v", marker)
	}
	for i := 1; i < population; i++ {
		id := fmt.Sprintf("pending-%04d", i)
		sum := sha256.Sum256([]byte(id))
		directory := fmt.Sprintf("wt-%x", sum[:12])
		marker["run_id"], marker["directory"] = id, directory
		delete(marker, "branch") // Only the real template acquired a Git branch.
		data, err := json.Marshal(marker)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repoRoot, "runs", directory), 0755); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(repoRoot, "markers", id+".json"), filepath.Join(repoRoot, "owners", directory+".json")} {
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
