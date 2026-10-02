//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationWorkerRecoveryPreservesSourceWithoutRunCustody(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	fixture := newWorkerRecoveryFixture(t, "worker-source")
	workspace := fixture.workspace
	file := filepath.Join(workspace.Path(), "implementation.txt")
	if err := os.WriteFile(file, []byte("uncommitted worker implementation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Remove(ctx); !errors.Is(err, worktree.ErrCleanupDeferred) {
		t.Fatalf("cleanup without run custody: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "uncommitted worker implementation" {
		t.Fatalf("worker source lost: %q %v", data, err)
	}
	entries, err := recovery.ReadInventory(ctx, filepath.Join(fixture.layout.Root, "recovery"), 128)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invented custody without journal: %v %v", entries, err)
	}
	run := fixture.createRun(t)
	defer func() { _ = run.Close() }()
	if err := workspace.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err = recovery.ReadInventory(ctx, filepath.Join(fixture.layout.Root, "recovery"), 128)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ordinary running worker cleanup created recovery entries: %v %v", entries, err)
	}
	if _, err := os.Stat(workspace.Path()); !os.IsNotExist(err) {
		t.Fatalf("acknowledged worker cleanup did not remove worktree: %v", err)
	}
}

func TestIntegrationWorkerRecoveryRetainsEscalatedCleanup(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	fixture := newWorkerRecoveryFixture(t, "worker-escalated")
	workspace := fixture.workspace
	if err := os.WriteFile(filepath.Join(workspace.Path(), "implementation.txt"), []byte("uncommitted worker implementation"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := fixture.createRun(t)
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := recovery.ReadInventory(ctx, filepath.Join(fixture.layout.Root, "recovery"), 128)
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing escalated worker archive: %v %v", entries, err)
	}
	destination := t.TempDir()
	recoveryCLIGit(t, destination, "init", "--initial-branch=main")
	// An independent host that already tracks the base branch, the same
	// property a real managed mirror has, is what makes a delta bundle
	// restorable here. destination has main checked out (even unborn), so
	// fetch into a distinct ref rather than refs/heads/main.
	recoveryCLIGit(t, destination, "fetch", fixture.source, "main:refs/heads/mirrored-base")
	entry := entries[0]
	if err := recovery.ImportSnapshotBundle(ctx, destination, filepath.Join(filepath.Dir(entry.RecordPath), recovery.BundleFileName), entry.Record, 512<<20); err != nil {
		t.Fatal(err)
	}
	if got := recoveryCLIGit(t, destination, "show", entry.Record.Ref+":implementation.txt"); got != "uncommitted worker implementation" {
		t.Fatalf("worker archive content: %q", got)
	}
}

type workerRecoveryFixture struct {
	layout    instance.Layout
	runID     string
	source    string
	workspace engine.Workspace
}

func newWorkerRecoveryFixture(t *testing.T, runID string) workerRecoveryFixture {
	t.Helper()
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clone := func(apiv1.RepoRef) (string, error) { return source, nil }
	option, err := recoveryCleanupOption(layout, cfg, manager.Root, clone, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	provisioner := workerhost.WorktreeWorkspaces{Manager: manager, CloneURL: clone}
	workspace, err := provisioner.Provision(context.Background(), engine.WorkspaceRequest{
		RunID: runID, Stage: "implement", Workflow: "implementation",
		RepoRef: apiv1.RepoRef{Provider: apiv1.Provider(cfg.Repos[0].Provider), Owner: cfg.Repos[0].Owner, Name: cfg.Repos[0].Name, Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return workerRecoveryFixture{layout: layout, runID: runID, source: source, workspace: workspace}
}

func (f workerRecoveryFixture) createRun(t *testing.T) *journal.Run {
	t.Helper()
	run, err := journal.Create(f.layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: f.runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
