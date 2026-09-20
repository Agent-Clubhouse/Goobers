//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

const (
	guardScopeFinalizedRunID = "guard-scope-finalized"
	guardScopeRunningRunID   = "guard-scope-running"
)

// TestIntegrationRecoveryCleanupGuardScopePerRun pins the daemon-lifetime
// property the terminal recovery guard used to break: one process finalizes
// many runs on one shared worktree manager, and the first finalization
// replaces that manager's recovery guard by name. If "terminal" travelled on
// the guard rather than being read from the owning run's journal, every later
// stage cleanup of every still-running run would take the terminal path and
// the clean-intermediate skip would never fire again.
func TestIntegrationRecoveryCleanupGuardScopePerRun(t *testing.T) {
	testdep.Require(t, "git")
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source, workcopies := t.TempDir(), t.TempDir()
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previousCloneURL })
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")

	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	option, err := recoveryCleanupOption(layout, cfg, workcopies, repoCloneURL, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)

	// The first run on this daemon reaches terminal: finalizeTerminalRun
	// installs the terminal recovery guard on the live runner manager.
	finalizeGuardScopeRun(t, layout, manager, guardScopeFinalizedRunID, time.Now().UTC().Add(-2*time.Hour))
	if records := readTerminalCaptureRecords(t, layout); len(records) != 0 {
		t.Fatalf("first finalization published %d recovery records, want none", len(records))
	}

	finishedAt := time.Now().UTC().Truncate(time.Second)
	running, branch := seedGuardScopeRunningRun(t, layout, finishedAt)
	defer func() { _ = running.Close() }()

	// A different run, still running, hits an ordinary stage boundary with its
	// work committed on its own branch and nothing else in the tree.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workspace, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: source, RunID: guardScopeRunningRunID + "-stage", OwnerRunID: guardScopeRunningRunID,
		BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "implementation.txt"), []byte("recover me"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, workspace.Path, "add", "implementation.txt")
	recoveryCLIGit(t, workspace.Path, "commit", "-m", "Implement feature")
	if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("stage cleanup did not remove the worktree: %v", err)
	}
	if records := readTerminalCaptureRecords(t, layout); len(records) != 0 {
		t.Fatalf("committed stage boundary after a finalization published %d recovery records, want none", len(records))
	}

	// The same run then turns terminal: the terminal capture path is unchanged.
	if err := running.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	standalone, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeTerminalRunWithClaimRelease(layout, nil, standalone, guardScopeRunningRunID,
		func(instance.Layout, *journal.InstanceLog, string) error { return nil }); err != nil {
		t.Fatalf("terminal finalization: %v", err)
	}
	assertGuardScopeTerminalCapture(t, layout, finishedAt)
}

// seedGuardScopeRunningRun creates a still-running run whose journal clock is
// the fixture's own finish stamp, so the terminal capture's anchor can be
// asserted without consulting the wall clock. It returns the run branch the
// runner would have recorded, which is what terminal capture reads.
func seedGuardScopeRunningRun(t *testing.T, layout instance.Layout, finishedAt time.Time) (*journal.Run, string) {
	t.Helper()
	branch := "goobers/implementation/" + guardScopeRunningRunID
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: guardScopeRunningRunID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: finishedAt.Add(-time.Hour),
	}, nil, journal.WithClock(func() time.Time { return finishedAt }))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: branch},
	}); err != nil {
		_ = run.Close()
		t.Fatal(err)
	}
	return run, branch
}

func assertGuardScopeTerminalCapture(t *testing.T, layout instance.Layout, finishedAt time.Time) {
	t.Helper()
	records := readTerminalCaptureRecords(t, layout)
	if len(records) != 1 {
		t.Fatalf("terminal finalization published %d recovery records, want 1", len(records))
	}
	if records[0].RunID != guardScopeRunningRunID {
		t.Fatalf("record belongs to run %q", records[0].RunID)
	}
	// The fixture's own finish stamp, never the wall clock: a terminal capture
	// is anchored to the durable finish event, a stage capture to the start.
	if !records[0].CreatedAt.Equal(finishedAt) {
		t.Fatalf("record createdAt = %s, want the run's finish event %s", records[0].CreatedAt, finishedAt)
	}
}

// finalizeGuardScopeRun drives a run with no worktree of its own to terminal
// through the shared manager, which is all it takes to replace that manager's
// recovery cleanup guard.
func finalizeGuardScopeRun(t *testing.T, layout instance.Layout, manager *worktree.Manager, runID string, startedAt time.Time) {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := finalizeTerminalRunWithClaimRelease(layout, nil, manager, runID,
		func(instance.Layout, *journal.InstanceLog, string) error { return nil }); err != nil {
		t.Fatalf("finalize run %s: %v", runID, err)
	}
}
