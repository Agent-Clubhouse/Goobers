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
	option, err := recoveryCleanupOption(layout, cfg, manager.Root, clone, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	provisioner := workerhost.WorktreeWorkspaces{Manager: manager, CloneURL: clone}
	ctx := context.Background()
	workspace, err := provisioner.Provision(ctx, engine.WorkspaceRequest{RunID: "worker-source", Stage: "implement", Workflow: "implementation", RepoRef: apiv1.RepoRef{Provider: apiv1.Provider(cfg.Repos[0].Provider), Owner: cfg.Repos[0].Owner, Name: cfg.Repos[0].Name, Branch: "main"}})
	if err != nil {
		t.Fatal(err)
	}
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
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invented custody without journal: %v %v", entries, err)
	}
	// A shared-journal worker can retry once authoritative run metadata is
	// available. A remote worker needs a transport for this handoff; this
	// fixture intentionally does not pretend to implement that transport.
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: "worker-source", Workflow: "implementation", WorkflowVersion: 1, StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if err := workspace.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err = recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing acknowledged archive: %v %v", entries, err)
	}
	destination := t.TempDir()
	recoveryCLIGit(t, destination, "init", "--initial-branch=main")
	// An independent host that already tracks the base branch, the same
	// property a real managed mirror has, is what makes a delta bundle
	// restorable here. destination has main checked out (even unborn), so
	// fetch into a distinct ref rather than refs/heads/main.
	recoveryCLIGit(t, destination, "fetch", source, "main:refs/heads/mirrored-base")
	entry := entries[0]
	if err := recovery.ImportSnapshotBundle(ctx, destination, filepath.Join(filepath.Dir(entry.RecordPath), recovery.BundleFileName), entry.Record, 512<<20); err != nil {
		t.Fatal(err)
	}
	if got := recoveryCLIGit(t, destination, "show", entry.Record.Ref+":implementation.txt"); got != "uncommitted worker implementation" {
		t.Fatalf("worker archive content: %q", got)
	}
}
