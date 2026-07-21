package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/worktree"
)

func TestPruneConfiguredRetentionDefaultsOffThenDryRunsAndDeletes(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	const runID = "retention-failure"
	createTerminalRun(t, layout, runID)
	manager, repo := commandWorktreeFixture(t, layout)
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: runID + "-stage", OwnerRunID: runID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := wt.Remove(context.Background(), worktree.RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("keep worktree: %v", err)
	}
	setup := &schedulerSetup{
		Config:          &instance.Config{},
		LegacyWorktrees: manager,
	}

	var stdout, stderr bytes.Buffer
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("disabled prune: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("disabled retention output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("default retention removed worktree: %v", err)
	}

	setup.Config.Retention = instance.RetentionConfig{DryRun: true, MaxRetainedWorktreeBytes: 1}
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("dry-run prune: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "retention candidate rule=storage-cap kind=worktree") {
		t.Fatalf("dry-run output = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("dry-run stderr = %q", stderr.String())
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("dry-run removed worktree: %v", err)
	}

	stdout.Reset()
	setup.Config.Retention = instance.RetentionConfig{Enabled: true, MaxRetainedWorktreeBytes: 1}
	if err := pruneConfiguredRetention(context.Background(), layout, setup, &stdout, &stderr); err != nil {
		t.Fatalf("enabled prune: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "retention deleted rule=storage-cap kind=worktree") ||
		!strings.Contains(got, "reclaimedBytes=") {
		t.Fatalf("delete output = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("delete stderr = %q", stderr.String())
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("enabled retention left worktree: %v", err)
	}
}
