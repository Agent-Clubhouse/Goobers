package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// slowDeterministicWorkflowYAML mirrors deterministicWorkflowYAML (daemon_test.go)
// but sleeps long enough that a real `goobers up` daemon holds the run's
// journal lock (see internal/journal/run.go's acquireRunLock doc) for several
// seconds — the window #2270's tests need to race `run abort` against a live
// daemon's lock.
const slowDeterministicWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@every 24h"
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: run a slow no-op local command so the run stays live long enough
        to abort while it is in flight
      run:
        command: ["sleep", "5"]
`

// initSlowDeterministicDemo is initDeterministicDemo (daemon_test.go), but
// with a task that blocks for several seconds instead of returning instantly.
func initSlowDeterministicDemo(t *testing.T) string {
	t.Helper()
	root := initDemo(t)

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(slowDeterministicWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "config", "gaggles", "example", "goobers")); err != nil {
		t.Fatal(err)
	}

	fixtureRepo := newDaemonFixtureRepo(t)
	prev := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }
	t.Cleanup(func() { repoCloneURL = prev })

	return root
}

// TestRunAbortDelegatesToLiveDaemonOnLockContention is #2270's end-to-end
// happy path: `run abort` against a run a real live `goobers up` daemon is
// actively executing (and so holds the run's journal lock for) must delegate
// to the daemon's live-cancel path automatically instead of surfacing the
// journal's 30s lock-timeout error, the same way `run <workflow>` already
// auto-delegates triggering to a live daemon (#343).
func TestRunAbortDelegatesToLiveDaemonOnLockContention(t *testing.T) {
	restoreLock := journal.SetLockTimeoutForTest(500*time.Millisecond, 20*time.Millisecond)
	t.Cleanup(restoreLock)
	prevSweep := delegationSweepInterval
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = prevSweep })

	root := initSlowDeterministicDemo(t)
	l := instance.NewLayout(root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upStdout := &daemonStartedWriter{started: make(chan struct{})}
	var upStderr bytes.Buffer
	var upCode int
	upDone := make(chan struct{})
	go func() {
		upCode = runUpContext(ctx, []string{root}, upStdout, &upStderr)
		close(upDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-upDone:
		case <-time.After(10 * time.Second):
			t.Error("runUpContext did not shut down during cleanup")
		}
	})

	select {
	case <-upStdout.started:
	case <-upDone:
		t.Fatalf("runUpContext exited before startup: code = %d, stderr = %q", upCode, upStderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for runUpContext to report daemon readiness")
	}

	code, stdout, stderr := runArgs(t, "run", "default-implement", "--no-wait", root)
	if code != 0 {
		t.Fatalf("run --no-wait: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	runID := runIDFromRunStdout(t, stdout)

	// Wait for the run to actually be live under the daemon (holding its
	// journal lock via acquireRunLock) before racing abort against it.
	waitForRunPhase(t, l.RunsDir(), runID, journal.PhaseRunning)

	code, stdout, stderr = runArgs(t, "run", "abort", runID, root)
	if code != 0 {
		t.Fatalf("run abort: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "delegated to live daemon") {
		t.Fatalf("stdout = %q, want a mention of live-daemon delegation", stdout)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	phase, err := waitForRunTerminal(waitCtx, l.RunsDir(), runID)
	if err != nil {
		t.Fatalf("wait for aborted run: %v", err)
	}
	if phase != journal.PhaseAborted {
		t.Fatalf("phase = %s, want aborted", phase)
	}
}

// TestRunAbortFallsBackToJournalErrorWithoutLiveDaemon guards the regression
// #2270 must not introduce: a run's journal lock can be contended by
// something other than a live `goobers up` daemon (e.g. two concurrent CLI
// invocations), and in that case there is nothing to safely delegate to —
// `run abort` must keep surfacing the original journal.ErrLockTimeout error
// unchanged rather than silently doing nothing or misreporting success. This
// hand-builds a live run the same way runcancel_test.go's
// TestSweepCancelAbortsLiveTrackedRun does (a runner.Runner blocked on a
// deterministic task), which holds the run's real journal lock without ever
// starting a `goobers up` daemon — so no up.lock exists for this layout.
func TestRunAbortFallsBackToJournalErrorWithoutLiveDaemon(t *testing.T) {
	restoreLock := journal.SetLockTimeoutForTest(300*time.Millisecond, 20*time.Millisecond)
	t.Cleanup(restoreLock)

	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	deterministic := &liveStalledDeterministic{started: make(chan struct{})}
	runRunner, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		Worktrees:  manager,
		ScratchDir: filepath.Join(layout.WorkcopiesDir(), "scratch"),
		RunsDir:    layout.RunsDir(),
		FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
			return finalizeTerminalRun(layout, log, manager, runID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runners := newDaemonRunnerRegistry()
	runners.Replace(map[string]*runner.Runner{"example": runRunner})
	machine, err := workflow.Compile(workflow.Definition{
		Name: "implementation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example",
			Start:  "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "block until cancelled",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: workflow.TerminalComplete,
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	var tracked sync.WaitGroup
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow:  "implementation",
		Gaggle:    "example",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   &trackedStarter{r: runRunner, machine: machine, wg: &tracked, l: layout, log: log, runners: runners},
	}}, log)
	runID, err := sched.Trigger(context.Background(), "implementation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-deterministic.started:
	case <-time.After(5 * time.Second):
		t.Fatal("live run did not enter its attempt")
	}
	t.Cleanup(func() {
		_, _, _ = runRunner.CancelRun(runID, time.Now())
		sched.Wait()
		tracked.Wait()
	})

	code, stdout, stderr := runArgs(t, "run", "abort", runID, layout.Root)
	if code != 2 {
		t.Fatalf("code = %d, want 2, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "lock held by another process") {
		t.Fatalf("stderr = %q, want the original journal lock-timeout error", stderr)
	}
}
