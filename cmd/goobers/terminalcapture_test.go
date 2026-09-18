package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestCaptureTerminalRunBranchWithoutJournaledBranchSkipsConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config []byte
	}{
		{name: "missing"},
		{name: "invalid", config: []byte("repos: [")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			if tc.config != nil {
				if err := os.WriteFile(layout.ConfigFile(), tc.config, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			const runID = "terminal-without-branch"
			run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
				Schema: journal.RunSchema, RunID: runID, Workflow: "implementation",
				WorkflowVersion: 1, Gaggle: "example", StartedAt: time.Now().UTC(),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			manager, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
			if err != nil {
				t.Fatal(err)
			}
			if err := captureTerminalRunBranch(layout, manager, runID); err != nil {
				t.Fatalf("capture without a journaled branch: %v", err)
			}
		})
	}
}
