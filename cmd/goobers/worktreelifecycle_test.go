package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

type liveAgenticAttempt struct {
	workspace string
	returned  chan struct{}
}

const abortAgenticWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: acceptance
spec:
  gaggle: example
  triggers:
    - type: backlog-item
      selector:
        goobers: "true"
  readiness:
    maxConcurrentRuns: 1
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: implementer
      goal: Wait until the daemon begins draining, then finish into the abort target.
      capabilities:
        - repo:push
      next: "@abort"
`

func TestDaemonDrainMidAgenticStageFinalizesOwnedWorktrees(t *testing.T) {
	root := initAcceptanceDemo(t)
	l := instance.NewLayout(root)
	writeFixture(t, filepath.Join(root, "config", "gaggles", "example", "workflows", "acceptance.yaml"), abortAgenticWorkflowYAML)
	baseline := worktreeDirectoryCount(t, l.WorkcopiesDir())

	previousSweepInterval := delegationSweepInterval
	delegationSweepInterval = 10 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = previousSweepInterval })

	started := make(chan liveAgenticAttempt, 4)
	proceed := make(chan struct{})
	previousAdapter := newAgenticAdapter
	newAgenticAdapter = func(string, map[string]string) harness.Adapter {
		return &harness.FakeAdapter{Act: func(_ context.Context, req harness.RunRequest) error {
			returned := make(chan struct{})
			started <- liveAgenticAttempt{workspace: req.Workspace, returned: returned}
			<-proceed
			err := harness.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "fixture agent completed during daemon drain",
			})
			close(returned)
			return err
		}}
	}
	t.Cleanup(func() { newAgenticAdapter = previousAdapter })
	t.Cleanup(func() { close(proceed) })

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		var stdout, stderr bytes.Buffer
		daemonDone := make(chan int, 1)
		go func() { daemonDone <- runUpContext(ctx, []string{root}, &stdout, &stderr) }()

		requestID, err := writeTriggerRequest(l.SchedulerDir(), "acceptance")
		if err != nil {
			cancel()
			t.Fatalf("cycle %d write trigger request: %v", i, err)
		}
		runID, err := pollTriggerResponse(context.Background(), l.SchedulerDir(), requestID, 5*time.Second)
		if err != nil {
			cancel()
			t.Fatalf("cycle %d trigger run: %v", i, err)
		}

		var attempt liveAgenticAttempt
		select {
		case attempt = <-started:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("cycle %d agentic stage did not start", i)
		}
		if _, err := os.Stat(attempt.workspace); err != nil {
			cancel()
			t.Fatalf("cycle %d live worktree: %v", i, err)
		}

		cancel()
		proceed <- struct{}{}
		select {
		case <-attempt.returned:
		case <-time.After(5 * time.Second):
			t.Fatalf("cycle %d agentic stage did not return", i)
		}
		select {
		case code := <-daemonDone:
			if code != 0 {
				t.Fatalf("cycle %d daemon exit code = %d, stderr=%q", i, code, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("cycle %d daemon did not drain", i)
		}

		waitForInstanceRunFinished(t, l.SchedulerDir(), runID, journal.PhaseAborted)
		assertRunFinishedLast(t, l.RunsDir(), runID, journal.PhaseAborted)
		if _, err := os.Stat(attempt.workspace); !os.IsNotExist(err) {
			t.Fatalf("cycle %d aborted run's worktree still exists: %v", i, err)
		}
		if got := worktreeDirectoryCount(t, l.WorkcopiesDir()); got != baseline {
			t.Fatalf("cycle %d worktree count = %d, want baseline %d", i, got, baseline)
		}
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
	assertRunFinishedLast(t, l.RunsDir(), runID, journal.PhaseAborted)
}

func TestUpReapsTerminalDeregisteredOrphanAndKeepsMarkedWorktree(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const orphanRunID = "startup-terminal-orphan"
	const keptRunID = "startup-kept-worktree"
	const activeRunID = "startup-active-terminal-worktree"
	createTerminalRun(t, l, orphanRunID)
	createTerminalRun(t, l, keptRunID)
	createTerminalRun(t, l, activeRunID)

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
	active, err := wtMgr.Create(context.Background(), worktree.CreateOptions{
		RepoURL: repo, RunID: activeRunID + "-implement", OwnerRunID: activeRunID, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("Create active terminal worktree: %v", err)
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
	if _, err := os.Stat(active.Path); !os.IsNotExist(err) {
		t.Fatalf("startup terminal finalizer left active worktree: %v", err)
	}
	if got := countKeptAnnotations(t, l, keptRunID, kept.RunID); got != 1 {
		t.Fatalf("startup kept annotations = %d, want 1", got)
	}
	assertRunFinishedLast(t, l.RunsDir(), keptRunID, journal.PhaseAborted)
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
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatalf("ReadInstanceLog: %v", err)
	}
	var count int
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.RunID == runID &&
			event.Runner["worktreeID"] == worktreeID &&
			event.Runner["worktreeStatus"] == "kept" {
			count++
		}
	}
	return count
}

func assertRunFinishedLast(t *testing.T, runsDir, runID string, phase journal.RunPhase) {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("run journal is empty")
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished || last.Status != string(phase) {
		t.Fatalf("last run event = (%s, %s), want (run.finished, %s)", last.Type, last.Status, phase)
	}
}

func waitForInstanceRunFinished(t *testing.T, schedulerDir, runID string, phase journal.RunPhase) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, err := journal.ReadInstanceLog(schedulerDir)
		if err == nil {
			for _, event := range events {
				if event.Type == journal.EventRunFinished && event.RunID == runID {
					if event.Status != string(phase) {
						t.Fatalf("instance run.finished status = %q, want %q", event.Status, phase)
					}
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance journal did not finish run %s", runID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func worktreeDirectoryCount(t *testing.T, workcopiesDir string) int {
	t.Helper()
	repos, err := os.ReadDir(workcopiesDir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read workcopies: %v", err)
	}
	var count int
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		runs, err := os.ReadDir(filepath.Join(workcopiesDir, repo.Name(), "runs"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read worktree runs: %v", err)
		}
		for _, run := range runs {
			if run.IsDir() {
				count++
			}
		}
	}
	return count
}
