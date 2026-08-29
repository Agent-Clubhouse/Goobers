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

// TestDaemonRunnerRegistryRunIDsReflectsTracking is issue #2014's liveness
// signal at its source: RunIDs must list exactly the runs this process is
// currently tracking, and drop one the moment its untrack func runs — the
// same bracket Track/untrack already use around Start/Resume, so a run that
// finishes or a process that dies stops appearing without renewLiveClaims
// needing any separate notification of either.
func TestDaemonRunnerRegistryRunIDsReflectsTracking(t *testing.T) {
	r := newDaemonRunnerRegistry()
	if ids := r.RunIDs(); len(ids) != 0 {
		t.Fatalf("RunIDs() = %v, want none tracked yet", ids)
	}

	owner := &runner.Runner{}
	untrackA := r.Track("run-a", "workflow-a", owner)
	untrackB := r.Track("run-b", "workflow-b", owner)

	ids := r.RunIDs()
	if len(ids) != 2 {
		t.Fatalf("RunIDs() = %v, want run-a and run-b", ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["run-a"] || !seen["run-b"] {
		t.Fatalf("RunIDs() = %v, want run-a and run-b", ids)
	}

	untrackA()
	ids = r.RunIDs()
	if len(ids) != 1 || ids[0] != "run-b" {
		t.Fatalf("RunIDs() after untracking run-a = %v, want only run-b", ids)
	}

	untrackB()
	if ids := r.RunIDs(); len(ids) != 0 {
		t.Fatalf("RunIDs() after untracking both = %v, want none", ids)
	}
}

// TestDaemonRunnerRegistryRunIDsNilSafe mirrors Resolve/Track's own nil
// receiver tolerance (a *daemonRunnerRegistry is optional in several
// construction paths) — RunIDs must not panic when called on one.
func TestDaemonRunnerRegistryRunIDsNilSafe(t *testing.T) {
	var r *daemonRunnerRegistry
	if ids := r.RunIDs(); len(ids) != 0 {
		t.Fatalf("RunIDs() on nil registry = %v, want none", ids)
	}
}

func TestDaemonRunnerRegistryHardStopsRunTrackedAfterForce(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
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
	})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "implementation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example",
			Start:  "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "must be stopped",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: workflow.TerminalComplete,
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	registry := newDaemonRunnerRegistry()
	registry.HardStopAll(nil)
	untrack := registry.Track("late-run", "implementation", runRunner)
	defer untrack()
	result, err := runRunner.Start(context.Background(), runner.StartInput{
		RunID:   "late-run",
		Machine: machine,
		Gaggle:  "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil || result.Phase != journal.PhaseRunning {
		t.Fatalf("late tracked Start() = %+v, %v, want running checkpoint", result, err)
	}
}

func TestDaemonRunnerRegistryReportsHardStopCountAtForceActivation(t *testing.T) {
	registry := newDaemonRunnerRegistry()
	owner := &runner.Runner{}
	untrack := registry.Track("active-run", "implementation", owner)
	defer untrack()

	reporting := make(chan struct{})
	releaseReport := make(chan struct{})
	stopped := make(chan int, 1)
	go func() {
		stopped <- registry.HardStopAll(func(count int) {
			if count != 1 {
				t.Errorf("hard-stop count = %d, want 1", count)
			}
			close(reporting)
			<-releaseReport
		})
	}()
	<-reporting

	tracked := make(chan func(), 1)
	go func() {
		tracked <- registry.Track("late-run", "implementation", owner)
	}()
	select {
	case untrackLate := <-tracked:
		untrackLate()
		t.Fatal("Track completed before force activation and counting finished")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseReport)
	if count := <-stopped; count != 1 {
		t.Fatalf("HardStopAll() = %d, want 1", count)
	}
	untrackLate := <-tracked
	untrackLate()
}

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

func TestDaemonRunnerRegistryRetainsOverlappingLeases(t *testing.T) {
	first := &runner.Runner{}
	second := &runner.Runner{}
	registry := newDaemonRunnerRegistry()

	original := registry.Track("run-overlap", "", first)
	overlapping := registry.Track("run-overlap", "", first)
	original()
	original()
	if owner, live := registry.Resolve("run-overlap", "", nil); !live || owner != first {
		t.Fatalf("owner after first release = (%p, %t), want first owner live", owner, live)
	}
	overlapping()
	if owner, live := registry.Resolve("run-overlap", "", nil); live || owner != nil {
		t.Fatalf("owner after final release = (%p, %t), want no live owner", owner, live)
	}

	oldGeneration := registry.Track("run-replaced", "", first)
	newGeneration := registry.Track("run-replaced", "", second)
	oldGeneration()
	if owner, live := registry.Resolve("run-replaced", "", nil); !live || owner != second {
		t.Fatalf("owner after stale release = (%p, %t), want replacement owner live", owner, live)
	}
	newGeneration()
}

func TestDaemonRunnerRegistryRefusesIncompatibleInterventionOwner(t *testing.T) {
	first := &runner.Runner{}
	reloaded := &runner.Runner{}
	registry := newDaemonRunnerRegistry()
	release := registry.Track("run-live", "", first)
	defer release()
	registry.Replace(map[string]*runner.Runner{"example": reloaded})

	if owner, live := registry.Resolve("run-live", "example", reloaded); !live || owner != first {
		t.Fatalf("resolved owner = (%p, %t), want retained live owner", owner, live)
	}
	if untrack, ok := registry.TrackCompatible("run-live", reloaded); ok {
		untrack()
		t.Fatal("incompatible intervention replaced the live owner")
	}
	compatible, ok := registry.TrackCompatible("run-live", first)
	if !ok {
		t.Fatal("compatible intervention owner was refused")
	}
	compatible()
}

func TestSweepStalledRunsEscalatesLiveAdmittedRunAcrossReload(t *testing.T) {
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
		0,
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
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var finished int
	for _, event := range events {
		if event.Type == journal.EventRunFinished && event.RunID == runID {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("instance run.finished events for %s = %d, want 1", runID, finished)
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

	t.Cleanup(func() { _ = log.Close() })

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
		0,
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

func TestSweepStalledRunsUsesPinnedPerRunTimeout(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}
	eventTime := now.Add(-time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "short-timeout-run", "short", "", &eventTime, time.Time{}, &apiv1.RunControls{
		MaxRepasses:       3,
		StalledRunTimeout: "30m",
	})
	eventTime = now.Add(-time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "long-timeout-run", "long", "", &eventTime, time.Time{}, &apiv1.RunControls{
		MaxRepasses:       3,
		StalledRunTimeout: "2h",
	})

	if err := sweepStalledRuns(layout, nil, runRunner, nil, nil, nil, nil, now, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "short-timeout-run", journal.PhaseEscalated)
	assertWatchdogPhase(t, layout.RunsDir(), "long-timeout-run", journal.PhaseRunning)
}

func TestSweepStalledRunsAbortsOverAgeRunWithFreshHeartbeat(t *testing.T) {
	now := time.Date(2026, 8, 2, 20, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}

	started := now.Add(-3 * time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "expired-run", "implementation", "", &started, now.Add(-time.Minute), &apiv1.RunControls{
		MaxRepasses:       3,
		StalledRunTimeout: "45m",
		MaxRunDuration:    "2h",
	})
	started = now.Add(-3 * time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "duration-disabled", "implementation", "", &started, now.Add(-time.Minute), &apiv1.RunControls{
		MaxRepasses:       3,
		StalledRunTimeout: "45m",
	})

	if err := sweepStalledRuns(layout, nil, runRunner, nil, nil, nil, nil, now, 45*time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "expired-run", journal.PhaseAborted)
	assertWatchdogPhase(t, layout.RunsDir(), "duration-disabled", journal.PhaseRunning)

	reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), "expired-run"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-2].Error == nil ||
		events[len(events)-2].Error.Code != runner.RunDurationExceededErrorCode {
		t.Fatalf("terminal events = %+v, want duration-exceeded error", events)
	}
}

func TestSweepStalledRunsPreservesPausedHumanGate(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	eventTime := now.Add(-2 * time.Hour)
	layout := instance.NewLayout(t.TempDir())
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: "paused-run", Workflow: "implementation", WorkflowVersion: 1,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("approval")
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: "approval"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	released := false
	if err := sweepStalledRuns(
		layout,
		nil,
		nil,
		nil,
		nil,
		nil,
		func(string, string) { released = true },
		now,
		45*time.Minute,
		0,
	); err != nil {
		t.Fatal(err)
	}

	assertWatchdogPhase(t, layout.RunsDir(), "paused-run", journal.PhaseRunning)
	if released {
		t.Fatal("paused human gate was released by stalled-run sweep")
	}
}

// TestSweepStalledRunsPreservesPausedGateBehindAPodPlaneEmit is the daemon-side
// half of the same protection. A mode-3 stage emits into the run's own journal
// through the write API's journal plane (livejournal.Writer.Adopt appends on
// the runner's handle), so an event can land AFTER the runner's gate.paused.
// While this sweep tested only the LAST event, such a run read "not parked" and
// a gate held past the timeout was escalated and its claim released — work a
// human was still deciding on, destroyed by the watchdog.
func TestSweepStalledRunsPreservesPausedGateBehindAPodPlaneEmit(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	eventTime := now.Add(-2 * time.Hour)
	layout := instance.NewLayout(t.TempDir())
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: "paused-pod-run", Workflow: "implementation", WorkflowVersion: 1,
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil, journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("approval")
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: "approval"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordArtifactAnnotated("pr.json", []byte(`{"number":42}`),
		apiv1.IntegrityDerived, map[string]any{"emitKey": "paused-pod-run|0|open-pr|1|0"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	released := false
	if err := sweepStalledRuns(
		layout,
		nil,
		nil,
		nil,
		nil,
		nil,
		func(string, string) { released = true },
		now,
		45*time.Minute,
		0,
	); err != nil {
		t.Fatal(err)
	}

	assertWatchdogPhase(t, layout.RunsDir(), "paused-pod-run", journal.PhaseRunning)
	if released {
		t.Fatal("a gate still awaiting a human was escalated because a pod emit landed after gate.paused")
	}
}

func TestStalledRunSweepErrorsReachInstanceJournal(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

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
	reporter.report(sweepStalledRuns(layout, nil, nil, log, nil, nil, nil, now, 45*time.Minute, 0))

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

func TestSweepStalledRunsSkipsSpansOnlyRunDirectory(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(filepath.Join(layout.RunsDir(), "legacy-run", "spans"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sweepStalledRuns(layout, nil, nil, nil, nil, nil, nil, time.Now(), 45*time.Minute, 0); err != nil {
		t.Fatalf("sweep spans-only run directory: %v", err)
	}
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

	err := sweepStalledRuns(layout, nil, nil, nil, nil, nil, nil, time.Now(), 45*time.Minute, 0)
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
	t.Cleanup(func() { _ = log.Close() })

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
	if err := sweepStalledRuns(layout, runners, nil, log, prepare, notify, nil, now, 45*time.Minute, 0); err != nil {
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
	createWatchdogRunWithControls(t, runsDir, runID, workflow, gaggle, eventTime, heartbeat, nil)
}

func createWatchdogRunWithControls(t *testing.T, runsDir, runID, workflow, gaggle string, eventTime *time.Time, heartbeat time.Time, controls *apiv1.RunControls) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: workflow, WorkflowVersion: 1, Gaggle: gaggle,
		RunControls: controls,
		Trigger:     journal.Trigger{Kind: journal.TriggerManual},
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
