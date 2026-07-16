package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestRunAbortFinalizesOwnedWorktree(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "abort-worktree"
	newStuckRun(t, l, runID, "default-implement")

	wtMgr, repo := commandWorktreeFixture(t, l)
	wt, err := wtMgr.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: runID + "-implement", OwnerRunID: runID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if code, _, stderr := runArgs(t, "run", "abort", runID, root); code != 0 {
		t.Fatalf("run abort: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("aborted run's worktree still exists: %v", err)
	}
}

func TestRunAbortPreservesAndJournalsKeptWorktree(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "abort-kept-worktree"
	newStuckRun(t, l, runID, "default-implement")

	wtMgr, repo := commandWorktreeFixture(t, l)
	wt, err := wtMgr.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: runID + "-implement", OwnerRunID: runID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := wt.Remove(context.Background(), worktree.RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("Remove(Keep): %v", err)
	}

	if code, _, stderr := runArgs(t, "run", "abort", runID, root); code != 0 {
		t.Fatalf("run abort: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("kept worktree was removed: %v", err)
	}
	if got := countKeptAnnotations(t, l, runID, wt.RunID); got != 1 {
		t.Fatalf("kept annotations = %d, want 1", got)
	}

	if code, _, stderr := runArgs(t, "run", "abort", runID, root); code != 1 {
		t.Fatalf("second run abort: code=%d stderr=%q", code, stderr)
	}
	if got := countKeptAnnotations(t, l, runID, wt.RunID); got != 1 {
		t.Fatalf("kept annotations after idempotent finalization = %d, want 1", got)
	}
}

func TestRunAbortCyclesKeepWorktreeCountFlat(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	wtMgr, repo := commandWorktreeFixture(t, l)

	for i := 0; i < 3; i++ {
		runID := fmt.Sprintf("abort-cycle-%d", i)
		newStuckRun(t, l, runID, "default-implement")
		wt, err := wtMgr.Create(context.Background(), worktree.CreateOptions{
			RepoURL: repo, RunID: runID + "-implement", OwnerRunID: runID, BaseRef: "main",
		})
		if err != nil {
			t.Fatalf("cycle %d Create: %v", i, err)
		}
		runsDir := filepath.Dir(wt.Path)

		if code, _, stderr := runArgs(t, "run", "abort", runID, root); code != 0 {
			t.Fatalf("cycle %d run abort: code=%d stderr=%q", i, code, stderr)
		}
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			t.Fatalf("cycle %d read worktree runs: %v", i, err)
		}
		if len(entries) != 0 {
			t.Fatalf("cycle %d left %d worktree directories; want 0", i, len(entries))
		}
	}
}

func TestUpReapsTerminalDeregisteredOrphanAndKeepsMarkedWorktree(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const orphanRunID = "startup-terminal-orphan"
	const keptRunID = "startup-kept-worktree"
	createTerminalRun(t, l, orphanRunID)
	createTerminalRun(t, l, keptRunID)

	wtMgr, repo := commandWorktreeFixture(t, l)
	orphan, err := wtMgr.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: orphanRunID + "-implement", OwnerRunID: orphanRunID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create orphan: %v", err)
	}
	kept, err := wtMgr.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: keptRunID + "-implement", OwnerRunID: keptRunID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create kept: %v", err)
	}
	if err := kept.Remove(context.Background(), worktree.RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("Remove(Keep): %v", err)
	}

	repoRoot := filepath.Dir(filepath.Dir(orphan.Path))
	markerPath := filepath.Join(repoRoot, "markers", orphan.RunID+".json")
	if err := os.Remove(markerPath); err != nil {
		t.Fatalf("remove orphan marker: %v", err)
	}
	runFixtureGit(t, filepath.Join(repoRoot, "repo.git"),
		"-c", "safe.bareRepository=all", "worktree", "remove", "--force", orphan.Path)
	if err := os.MkdirAll(orphan.Path, 0o755); err != nil {
		t.Fatalf("recreate orphan directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphan.Path, "leftover"), []byte("orphan"), 0o644); err != nil {
		t.Fatalf("write orphan fixture: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := runUpContext(ctx, []string{root}, &stdout, &stderr); code != 0 {
		t.Fatalf("runUpContext: code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(orphan.Path); !os.IsNotExist(err) {
		t.Fatalf("terminal deregistered orphan still exists: %v", err)
	}
	if _, err := os.Stat(kept.Path); err != nil {
		t.Fatalf("kept worktree was removed at startup: %v", err)
	}
}

func commandWorktreeFixture(t *testing.T, l instance.Layout) (*worktree.Manager, string) {
	t.Helper()
	repo, err := repoCloneURL(apiv1.RepoRef{})
	if err != nil {
		t.Fatalf("repoCloneURL: %v", err)
	}
	wtMgr, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return wtMgr, repo
}

func createTerminalRun(t *testing.T, l instance.Layout, runID string) {
	t.Helper()
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "default-implement", WorkflowVersion: 1,
		Gaggle: "example", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)}); err != nil {
		t.Fatalf("append run.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func countKeptAnnotations(t *testing.T, l instance.Layout, runID, worktreeID string) int {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var count int
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner["worktreeID"] == worktreeID &&
			event.Runner["worktreeStatus"] == "kept" {
			count++
		}
	}
	return count
}
