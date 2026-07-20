package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
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
	reporter.report(sweepStalledRuns(layout, nil, nil, log, nil, now, 45*time.Minute))

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

func createWatchdogRun(t *testing.T, runsDir, runID, workflow string, eventTime *time.Time, heartbeat time.Time) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: workflow, WorkflowVersion: 1,
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
