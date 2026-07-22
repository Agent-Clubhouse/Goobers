package main

import (
	"context"
	"encoding/json"
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

// TestSweepCancelAbortsLiveTrackedRun is #831's end-to-end happy path: a cancel
// request routed through the daemon sweep resolves the owning Runner, cancels
// the live run, finalizes it aborted, releases its backlog claim, and answers
// the waiting CLI with code=aborted — the full request → cancel → teardown loop.
func TestSweepCancelAbortsLiveTrackedRun(t *testing.T) {
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

	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithInstanceLog(log),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, holder, err := ledger.Claim("547", runID, "implementation", 24*time.Hour); err != nil || !ok {
		t.Fatalf("claim live run: ok=%v holder=%q err=%v", ok, holder, err)
	}

	requestID, err := writeCancelRequest(layout.SchedulerDir(), cancelRequest{
		RunID: runID, Workflow: "implementation", Gaggle: "example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sweepPendingCancelRequests(layout.SchedulerDir(), runners, log, sched.ReleaseRun, time.Now); err != nil {
		t.Fatal(err)
	}
	resp, err := pollCancelResponse(context.Background(), layout.SchedulerDir(), requestID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code != cancelCodeAborted || resp.Phase != string(journal.PhaseAborted) {
		t.Fatalf("response = %+v, want aborted", resp)
	}

	sched.Wait()
	tracked.Wait()

	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseAborted)
	reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 ||
		events[len(events)-2].Error == nil ||
		events[len(events)-2].Error.Code != runner.RunCanceledErrorCode ||
		events[len(events)-1].Type != journal.EventRunFinished ||
		events[len(events)-1].Status != string(journal.PhaseAborted) {
		t.Fatalf("terminal events = %+v, want run_canceled + run.finished(aborted)", events)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Lookup("547"); ok {
		t.Fatal("cancelled run's backlog claim was not released")
	}
}

// TestSweepCancelRespondsNotRunningForUnownedRun covers the discriminator: a
// cancel for a run no live owner is tracked for answers not_running rather than
// touching a journal.
func TestSweepCancelRespondsNotRunningForUnownedRun(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	runners := newDaemonRunnerRegistry()

	requestID, err := writeCancelRequest(layout.SchedulerDir(), cancelRequest{RunID: "01JZTESTNOTRUNNING000000000"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sweepPendingCancelRequests(layout.SchedulerDir(), runners, nil, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	resp, err := pollCancelResponse(context.Background(), layout.SchedulerDir(), requestID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code != cancelCodeNotRunning {
		t.Fatalf("response = %+v, want not_running", resp)
	}
}

// TestSweepCancelRefusesStaleRequest bounds a request the daemon never picked up
// in time: it is refused with an error rather than dispatched.
func TestSweepCancelRefusesStaleRequest(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	reqDir := filepath.Join(layout.SchedulerDir(), pendingCancelsDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cancelRequest{
		RunID:     "01JZTESTSTALEREQUEST0000000",
		CreatedAt: time.Now().Add(-2 * cancelDelegationTimeout).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reqDir, "stale"+cancelRequestSuffix), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := sweepPendingCancelRequests(layout.SchedulerDir(), newDaemonRunnerRegistry(), nil, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	resp, err := pollCancelResponse(context.Background(), layout.SchedulerDir(), "stale", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code == cancelCodeAborted || !strings.Contains(resp.Error, "stale") {
		t.Fatalf("response = %+v, want stale refusal", resp)
	}
}

// TestPollCancelResponseTimesOut bounds the wait when no daemon is sweeping.
func TestPollCancelResponseTimesOut(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	_, err := pollCancelResponse(context.Background(), layout.SchedulerDir(), "missing", 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
}

// TestRunCancelRequiresRunningDaemonPointsToAbort: with no daemon holding
// up.lock there is nothing in flight to cancel, so cancel refuses and names the
// offline repair path rather than editing the journal.
func TestRunCancelRequiresRunningDaemonPointsToAbort(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: "live-1", Workflow: "no-such-workflow", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "run", "cancel", "live-1", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "run abort") || !strings.Contains(stderr, "no `goobers up` daemon") {
		t.Fatalf("stderr = %q, want abort guidance", stderr)
	}
}

// TestRunCancelRejectsAlreadyTerminalRun: a finished run has nothing live to
// cancel.
func TestRunCancelRejectsAlreadyTerminalRun(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: "done-2", Workflow: "no-such-workflow", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "run", "cancel", "done-2", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "already terminal") {
		t.Fatalf("stderr = %q, want already-terminal", stderr)
	}
}

// TestRunCancelUnknownRun: an unresolvable run id is a usage/IO error.
func TestRunCancelUnknownRun(t *testing.T) {
	root := initDeterministicDemo(t)
	code, _, stderr := runArgs(t, "run", "cancel", "no-such-run", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2, stderr = %q", code, stderr)
	}
}
