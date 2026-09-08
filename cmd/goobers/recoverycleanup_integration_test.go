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
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryCleanupArchivesBeforeRemovingActiveRunWorktree(t *testing.T) {
	testdep.Require(t, "git")
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source, workcopies := t.TempDir(), t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	const runID = "cleanup-recovery"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example", StartedAt: time.Now().UTC()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the run's exclusive writer open throughout cleanup. Recovery may
	// read identity but must never try to reopen that writer.
	defer func() { _ = run.Close() }()
	option, err := recoveryCleanupOption(layout, cfg, workcopies, func(apiv1.RepoRef) (string, error) { return source, nil }, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(workcopies, option)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workspace, err := manager.Create(ctx, worktree.CreateOptions{RepoURL: source, RunID: runID + "-stage", OwnerRunID: runID, BaseRef: "main", Branch: "goobers/implementation/" + runID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "implementation.txt"), []byte("recover me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("acknowledged cleanup did not remove worktree: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(layout.Root, "recovery"))
	if err != nil {
		t.Fatal(err)
	}
	var retained recovery.Record
	for _, entry := range entries {
		if entry.IsDir() {
			retained, err = recovery.ReadRecord(filepath.Join(layout.Root, "recovery", entry.Name(), recovery.RecordFileName))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if retained.RunID != runID {
		t.Fatal("cleanup removed worktree without a bound recovery record")
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.RunID == runID && event.Runner["recoveryRef"] == retained.Ref {
			return
		}
	}
	t.Fatal("cleanup removed worktree without a recovery publication event")
}
