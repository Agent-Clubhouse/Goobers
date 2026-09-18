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

// TestIntegrationTerminalRunBranchCapture pins the terminal-time capture that
// closes the nonterminal/terminal cleanup gap: every worktree is gone before
// the run turns terminal, so the run branch in the mirror is the only place
// the implementation still exists.
func TestIntegrationTerminalRunBranchCapture(t *testing.T) {
	testdep.Require(t, "git")
	for _, tc := range []struct {
		name string
		// commit advances the run branch past the base.
		commit bool
		// untracked leaves harness-shaped debris behind, which makes the
		// stage cleanup publish a snapshot of that same HEAD — so the
		// terminal capture must find the branch tip already covered.
		untracked  bool
		wantRecord bool
	}{
		{name: "branch-ahead-of-base", commit: true, wantRecord: true},
		{name: "already-covered-by-stage-capture", commit: true, untracked: true, wantRecord: true},
		{name: "branch-at-base", wantRecord: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workcopies, startedAt, records := runTerminalBranchCaptureFixture(t, tc.commit, tc.untracked)
			assertTerminalCaptureLeavesNoScratch(t, workcopies)
			want := 0
			if tc.wantRecord {
				want = 1
			}
			if len(records) != want {
				t.Fatalf("terminal finalization published %d recovery records, want %d", len(records), want)
			}
			if want == 0 {
				return
			}
			if records[0].RunID != terminalCaptureRunID {
				t.Fatalf("record belongs to run %q", records[0].RunID)
			}
			if records[0].BaseRef != "refs/heads/main" {
				t.Fatalf("record base ref = %q, want the run's base", records[0].BaseRef)
			}
			// Which writer produced the single record is what separates the
			// two one-record cases, and asserting only the count would let
			// either one pass for the other's reason. A stage capture is
			// stamped with the run's start; a terminal capture with its
			// durable finish event, which the fixture seeds strictly later.
			// No wall clock is consulted: both are the fixture's own values.
			stageCapture := records[0].CreatedAt.Equal(startedAt)
			if stageCapture != tc.untracked {
				t.Fatalf("record createdAt = %s (run started %s): stage capture = %t, want %t",
					records[0].CreatedAt, startedAt, stageCapture, tc.untracked)
			}
		})
	}
}

const terminalCaptureRunID = "terminal-branch-capture"

// runTerminalBranchCaptureFixture drives one run all the way to terminal with
// its stage worktree removed while the run was still nonterminal, and returns
// every recovery record the whole lifecycle published.
func runTerminalBranchCaptureFixture(t *testing.T, commit, untracked bool) (string, time.Time, []recovery.Record) {
	t.Helper()
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

	branch := "goobers/implementation/" + terminalCaptureRunID
	startedAt := time.Now().UTC().Add(-time.Hour)
	run := seedTerminalCaptureRun(t, layout, branch, startedAt)
	defer func() { _ = run.Close() }()

	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	option, err := recoveryCleanupOption(layout, cfg, workcopies, repoCloneURL, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workspace, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: source, RunID: terminalCaptureRunID + "-stage", OwnerRunID: terminalCaptureRunID,
		BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if commit {
		if err := os.WriteFile(filepath.Join(workspace.Path, "implementation.txt"), []byte("recover me"), 0o600); err != nil {
			t.Fatal(err)
		}
		recoveryCLIGit(t, workspace.Path, "add", "implementation.txt")
		recoveryCLIGit(t, workspace.Path, "commit", "-m", "Implement feature")
	}
	if untracked {
		if err := os.WriteFile(filepath.Join(workspace.Path, "claimed-item.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The nonterminal teardown every stage boundary performs. The run is not
	// terminal yet, so this guard cannot know it is the last one.
	if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	standalone, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeTerminalRunWithClaimRelease(layout, nil, standalone, terminalCaptureRunID,
		func(instance.Layout, *journal.InstanceLog, string) error { return nil }); err != nil {
		t.Fatalf("terminal finalization: %v", err)
	}
	return workcopies, startedAt, readTerminalCaptureRecords(t, layout)
}

// seedTerminalCaptureRun creates the run journal and records the run branch the
// way the runner does, since that annotation is what terminal capture reads.
func seedTerminalCaptureRun(t *testing.T, layout instance.Layout, branch string, startedAt time.Time) *journal.Run {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: terminalCaptureRunID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: branch},
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

func readTerminalCaptureRecords(t *testing.T, layout instance.Layout) []recovery.Record {
	t.Helper()
	root := filepath.Join(layout.Root, "recovery")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var records []recovery.Record
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, err := recovery.ReadRetainedRecord(filepath.Join(root, entry.Name(), recovery.RecordFileName))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

// assertTerminalCaptureLeavesNoScratch proves the throwaway checkout capture
// materializes is gone afterwards. A leaked one would make the next capture,
// reap or retention pass argue about a worktree nothing owns — and, worse,
// leave a second copy of the implementation outside the inventory.
func assertTerminalCaptureLeavesNoScratch(t *testing.T, workcopies string) {
	t.Helper()
	repos, err := os.ReadDir(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		scratch := filepath.Join(workcopies, repo.Name(), terminalCaptureScratchDirectory)
		if _, err := os.Stat(scratch); !os.IsNotExist(err) {
			t.Fatalf("terminal capture left its scratch checkout at %s: %v", scratch, err)
		}
	}
}

// terminalCaptureScratchDirectory mirrors internal/worktree's own constant.
const terminalCaptureScratchDirectory = "terminal-capture"
