package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type stalledRunStarter struct {
	mu    sync.Mutex
	count int
}

func (s *stalledRunStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	return localscheduler.StartResult{Phase: journal.PhaseCompleted}, nil
}

type liveStalledDeterministic struct {
	started chan struct{}
	calls   atomic.Int32
}

func (d *liveStalledDeterministic) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if d.calls.Add(1) == 1 {
		close(d.started)
		<-ctx.Done()
		return apiv1.ResultEnvelope{}, ctx.Err()
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func TestSweepStalledRunsEscalatesLiveAdmittedRunAcrossReload(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	deterministic := &liveStalledDeterministic{started: make(chan struct{})}
	var notified []string
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
		NotifyTerminal: func(runID string, phase journal.RunPhase, _ string) error {
			if phase == journal.PhaseEscalated {
				notified = append(notified, runID+":"+string(phase))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reloadedRunner, err := runner.New(runner.Config{
		Worktrees: manager,
		RunsDir:   layout.RunsDir(),
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
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "block until reaped",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: workflow.TerminalComplete,
			}},
		},
	})
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
	if _, err := sched.Trigger(context.Background(), "implementation", time.Now()); err == nil ||
		!strings.Contains(err.Error(), localscheduler.ReasonMaxParallel) {
		t.Fatalf("trigger before live sweep error = %v, want max-parallel refusal", err)
	}

	runners.Replace(map[string]*runner.Runner{"example": reloadedRunner})
	now := time.Now().Add(2 * time.Hour)
	if err := sweepStalledRuns(
		layout,
		runners,
		reloadedRunner,
		log,
		nil,
		nil,
		sched.ReleaseRun,
		now,
		45*time.Minute,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Trigger(context.Background(), "implementation", now.Add(time.Second)); err != nil {
		t.Fatalf("trigger after live sweep: %v", err)
	}
	sched.Wait()
	tracked.Wait()

	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseEscalated)
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Lookup("547"); ok {
		t.Fatal("live stalled run claim was not released")
	}
	if len(notified) != 1 || notified[0] != runID+":"+string(journal.PhaseEscalated) {
		t.Fatalf("notifications = %v", notified)
	}
}

func TestSweepStalledRunsEscalatesSilentRunAndPreservesHeartbeat(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	timeout := 45 * time.Minute
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	staleID := "silent-run"
	staleTime := now.Add(-2 * time.Hour)
	createWatchdogRun(t, layout.RunsDir(), staleID, "implementation", &staleTime, time.Time{})

	heartbeatID := "heartbeat-run"
	started := now.Add(-3 * time.Hour)
	heartbeat := now.Add(-time.Minute)
	createWatchdogRun(t, layout.RunsDir(), heartbeatID, "long-running", &started, heartbeat)

	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithInstanceLog(log),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, holder, err := ledger.Claim("547", staleID, "implementation", 24*time.Hour); err != nil || !ok {
		t.Fatalf("claim stale run: ok=%v holder=%q err=%v", ok, holder, err)
	}

	var notified []string
	runRunner, err := runner.New(runner.Config{
		Worktrees: manager,
		RunsDir:   layout.RunsDir(),
		FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
			return finalizeTerminalRun(layout, log, manager, runID)
		},
		NotifyTerminal: func(runID string, phase journal.RunPhase, _ string) error {
			notified = append(notified, runID+":"+string(phase))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	starter := &stalledRunStarter{}
	sched := localscheduler.New([]localscheduler.WorkflowEntry{
		{
			Workflow:  "implementation",
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:   starter,
		},
		{
			Workflow:  "long-running",
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:   starter,
		},
	}, log)
	if err := sched.Reconcile(layout.RunsDir(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Trigger(context.Background(), "implementation", now); err == nil ||
		!strings.Contains(err.Error(), localscheduler.ReasonMaxParallel) {
		t.Fatalf("trigger before sweep error = %v, want max-parallel refusal", err)
	}

	if err := sweepStalledRuns(
		layout,
		nil,
		runRunner,
		log,
		nil,
		nil,
		sched.ReleaseReconciled,
		now,
		timeout,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := sched.Trigger(context.Background(), "implementation", now.Add(time.Second)); err != nil {
		t.Fatalf("trigger after sweep: %v", err)
	}
	sched.Wait()
	if len(notified) != 1 || notified[0] != staleID+":"+string(journal.PhaseEscalated) {
		t.Fatalf("notifications = %v", notified)
	}

	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Lookup("547"); ok {
		t.Fatal("stalled run claim was not released")
	}
	assertWatchdogPhase(t, layout.RunsDir(), staleID, journal.PhaseEscalated)
	assertWatchdogPhase(t, layout.RunsDir(), heartbeatID, journal.PhaseRunning)

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Type == journal.EventRunFinished && event.RunID == staleID &&
			event.Status == string(journal.PhaseEscalated) &&
			event.Error != nil && event.Error.Code == runner.RunStalledErrorCode {
			found = true
		}
	}
	if !found {
		t.Fatalf("instance journal has no run_stalled terminal event: %+v", events)
	}
}

func TestStalledRunSweepErrorsReachInstanceJournal(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	eventTime := now.Add(-time.Hour)
	createWatchdogRun(t, layout.RunsDir(), "broken-run", "implementation", &eventTime, time.Time{})
	eventsPath := filepath.Join(layout.RunsDir(), "broken-run", "events.jsonl")
	file, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{not-json}\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reporter := newSweepErrorReporter(log, "stalled_run_sweep_failed")
	reporter.report(sweepStalledRuns(layout, nil, nil, log, nil, nil, nil, now, 45*time.Minute))

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil &&
			event.Error.Code == "stalled_run_sweep_failed" {
			return
		}
	}
	t.Fatalf("instance journal has no stalled_run_sweep_failed event: %+v", events)
}

func TestStalledRunSweepReportsMissingRunIdentity(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if err := os.MkdirAll(filepath.Join(layout.RunsDir(), "missing-identity"), 0o755); err != nil {
		t.Fatal(err)
	}
	reporter := newSweepErrorReporter(log, "stalled_run_sweep_failed")
	reporter.report(sweepStalledRuns(layout, nil, nil, log, nil, nil, nil, time.Now(), 45*time.Minute))

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil &&
			event.Error.Code == "stalled_run_sweep_failed" &&
			strings.Contains(event.Error.Message, "missing-identity") {
			return
		}
	}
	t.Fatalf("instance journal has no missing run.yaml sweep failure: %+v", events)
}

func TestSweepStalledRunsReportsRunOpenFailure(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	runDir := filepath.Join(layout.RunsDir(), "unreadable-run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.yaml", filepath.Join(runDir, "run.yaml")); err != nil {
		t.Skipf("create run.yaml symlink loop: %v", err)
	}

	err := sweepStalledRuns(layout, nil, nil, nil, nil, nil, nil, time.Now(), 45*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "inspect run directory") {
		t.Fatalf("sweep error = %v, want run inspection failure", err)
	}
}

func TestSweepStalledRunsTerminalizesRemovedGaggleRoot(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	if err := layout.EnsureGaggleRuntime("removed"); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	runID := "removed-gaggle-run"
	eventTime := now.Add(-2 * time.Hour)
	createWatchdogRunForGaggle(t, layout.ForGaggle("removed").RunsDir(), runID, "implementation", "removed", &eventTime, time.Time{})
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithInstanceLog(log),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, holder, err := ledger.Claim("547", runID, "implementation", 24*time.Hour); err != nil || !ok {
		t.Fatalf("claim removed-gaggle run: ok=%v holder=%q err=%v", ok, holder, err)
	}

	var notified []string
	var prepared bool
	prepare := func(runLayout instance.Layout) (runner.TerminalPreparer, error) {
		if runLayout.Gaggle() != "removed" {
			t.Fatalf("terminal preparer layout gaggle = %q, want removed", runLayout.Gaggle())
		}
		return func(string, journal.RunPhase, *journal.Run) error {
			prepared = true
			return nil
		}, nil
	}
	notify := func(runID string, phase journal.RunPhase, _ string) error {
		notified = append(notified, runID+":"+string(phase))
		return nil
	}
	runners := newDaemonRunnerRegistry()
	if err := sweepStalledRuns(layout, runners, nil, log, prepare, notify, nil, now, 45*time.Minute); err != nil {
		t.Fatal(err)
	}

	assertWatchdogPhase(t, layout.ForGaggle("removed").RunsDir(), runID, journal.PhaseEscalated)
	if !prepared {
		t.Fatal("removed-gaggle stalled run skipped terminal preparation")
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Lookup("547"); ok {
		t.Fatal("removed-gaggle stalled run claim was not released")
	}
	if len(notified) != 1 || notified[0] != runID+":"+string(journal.PhaseEscalated) {
		t.Fatalf("notifications = %v", notified)
	}
}

func createWatchdogRun(t *testing.T, runsDir, runID, workflow string, eventTime *time.Time, heartbeat time.Time) {
	createWatchdogRunForGaggle(t, runsDir, runID, workflow, "", eventTime, heartbeat)
}

func createWatchdogRunForGaggle(t *testing.T, runsDir, runID, workflow, gaggle string, eventTime *time.Time, heartbeat time.Time) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: workflow, WorkflowVersion: 1, Gaggle: gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return *eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("implement")
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if !heartbeat.IsZero() {
		*eventTime = heartbeat
		if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "implement", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertWatchdogPhase(t *testing.T, runsDir, runID string, want journal.RunPhase) {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	phase, err := reader.Phase()
	if err != nil {
		t.Fatal(err)
	}
	if phase != want {
		t.Fatalf("run %s phase = %s, want %s", runID, phase, want)
	}
}
