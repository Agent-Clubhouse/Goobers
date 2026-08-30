package runner

import (
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// stalledWaitTimeout is the budget these tests give an async run/escalation
// to reach the state they're polling for. Windows CI runners are measurably
// slower at the process/goroutine-scheduling and file I/O this package
// exercises than POSIX ones (ci.yml's windows-smoke job documents the same
// finding for the package-level `go test` timeout), so 5s that's comfortable
// on Linux/macOS CI intermittently starved these tests of time on Windows.
// Widen instead of shrinking the margin further with the flake.
func stalledWaitTimeout() time.Duration {
	if runtime.GOOS == "windows" {
		return 20 * time.Second
	}
	return 5 * time.Second
}

type wedgedDeterministic struct {
	started chan struct{}
}

func (d *wedgedDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	close(d.started)
	select {}
}

type progressingGateReviewer struct {
	started  chan struct{}
	progress chan struct{}
	reported chan struct{}
	release  chan struct{}
}

func (r *progressingGateReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, nil
}

func (r *progressingGateReviewer) Review(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	close(r.started)
	for {
		select {
		case <-r.progress:
			invoke.ReportProgress(ctx)
			r.reported <- struct{}{}
		case <-r.release:
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		case <-ctx.Done():
			return apiv1.Verdict{}, context.Cause(ctx)
		}
	}
}

func waitForRunEvent(t *testing.T, runDir, description string, matches func(journal.Event) bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(stalledWaitTimeout())
	defer timeout.Stop()
	for {
		reader, err := journal.OpenRead(runDir)
		if err == nil {
			events, readErr := reader.Events()
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, event := range events {
				if matches(event) {
					return
				}
			}
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

func TestEscalateStalledUsesTerminalPath(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	eventTime := now.Add(-2 * time.Hour)
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "stalled-run", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("implement")
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	var finalized, notified bool
	r, err := New(Config{
		Worktrees: manager,
		RunsDir:   runsDir,
		FinalizeTerminal: func(runID string, phase journal.RunPhase) error {
			finalized = runID == "stalled-run" && phase == journal.PhaseEscalated
			return nil
		},
		NotifyTerminal: func(runID string, phase journal.RunPhase, finalState string) error {
			notified = runID == "stalled-run" && phase == journal.PhaseEscalated && finalState == "implement"
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, escalated, err := r.EscalateStalled("stalled-run", now, 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !escalated || result.Phase != journal.PhaseEscalated {
		t.Fatalf("escalated=%v result=%+v", escalated, result)
	}
	if !finalized || !notified {
		t.Fatalf("finalized=%v notified=%v", finalized, notified)
	}

	reader, err := journal.OpenRead(filepath.Join(runsDir, "stalled-run"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-2].Error == nil ||
		events[len(events)-2].Error.Code != RunStalledErrorCode ||
		events[len(events)-1].Type != journal.EventRunFinished ||
		events[len(events)-1].Status != string(journal.PhaseEscalated) {
		t.Fatalf("terminal events = %+v", events)
	}
}

func TestEscalateStalledRechecksLatestHeartbeat(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	eventTime := now.Add(-3 * time.Hour)
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "healthy-run", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	eventTime = now.Add(-time.Minute)
	if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := New(Config{Worktrees: manager, RunsDir: runsDir})
	if err != nil {
		t.Fatal(err)
	}
	result, escalated, err := r.EscalateStalled("healthy-run", now, 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if escalated || result.Phase != journal.PhaseRunning {
		t.Fatalf("escalated=%v result=%+v", escalated, result)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, "healthy-run"))
	if err != nil {
		t.Fatal(err)
	}
	if phase, err := reader.Phase(); err != nil || phase != journal.PhaseRunning {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
}

func TestEscalateStalledPreservesPausedHumanGate(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	eventTime := now.Add(-2 * time.Hour)
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{
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

	r, err := New(Config{Worktrees: manager, RunsDir: runsDir})
	if err != nil {
		t.Fatal(err)
	}
	result, escalated, err := r.EscalateStalled("paused-run", now, 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if escalated || result.Phase != journal.PhaseRunning {
		t.Fatalf("escalated=%v result=%+v", escalated, result)
	}
}

// TestEscalateStalledPreservesPausedGateBehindAPodPlaneEmit is the same
// protection, on the log a run-with-pods actually produces. A mode-3 stage
// emits into the run's own journal through the write API's journal plane
// (livejournal.Writer.Adopt appends on the runner's handle), so a retried emit,
// a late agent.lifecycle or — once gates are placeable — a pod-executed gate's
// artifacts can land AFTER the runner's gate.paused. Testing only the LAST
// event then read "not parked" while the run was still waiting for a human,
// and a gate held longer than the timeout was escalated: destructive, and no
// setting could prevent it.
func TestEscalateStalledPreservesPausedGateBehindAPodPlaneEmit(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	eventTime := now.Add(-2 * time.Hour)
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{
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
	// The pod-plane emit that lands after the pause, carrying the emit key the
	// journal plane stamps on everything it writes.
	if _, err := run.RecordArtifactAnnotated("pr.json", []byte(`{"number":42}`),
		apiv1.IntegrityDerived, map[string]any{"emitKey": "paused-pod-run|0|open-pr|1|0"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := New(Config{Worktrees: manager, RunsDir: runsDir})
	if err != nil {
		t.Fatal(err)
	}
	result, escalated, err := r.EscalateStalled("paused-pod-run", now, 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if escalated || result.Phase != journal.PhaseRunning {
		t.Fatalf("escalated=%v result=%+v; a run parked at a gate must survive a pod emit landing after the pause", escalated, result)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, "paused-pod-run"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	// The precondition that makes this test the one it claims to be: the last
	// event is NOT gate.paused, which is exactly why the old check failed.
	if last := events[len(events)-1].Type; last != journal.EventArtifactRecorded {
		t.Fatalf("last event = %s, want the pod emit to sit after gate.paused", last)
	}
	if phase, err := reader.Phase(); err != nil || phase != journal.PhaseRunning {
		t.Fatalf("phase = %s err = %v, want the run left running", phase, err)
	}
}

func TestEscalateStalledInterruptsRetryBackoff(t *testing.T) {
	flaky := &flakyDeterministic{failUntil: 100}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())
	machine := retryFixtureMachineWithBackoff(t, 3, 10*time.Second)

	type startOutcome struct {
		result Result
		err    error
	}
	done := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   "stalled-backoff",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub,
				Owner:    "acme",
				Name:     "web",
				Branch:   "main",
			},
		})
		done <- startOutcome{result: result, err: err}
	}()

	runDir := filepath.Join(runsDir, "stalled-backoff")
	waitForRunEvent(t, runDir, "run to enter retry backoff", func(event journal.Event) bool {
		return event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error"
	})

	start := time.Now()
	result, escalated, err := r.EscalateStalled("stalled-backoff", time.Now().Add(2*time.Hour), 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !escalated || result.Phase != journal.PhaseEscalated {
		t.Fatalf("escalated=%v result=%+v", escalated, result)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stalled backoff took %s to stop", elapsed)
	}

	outcome := <-done
	if outcome.err != nil || outcome.result.Phase != journal.PhaseEscalated {
		t.Fatalf("Start() = %+v, %v", outcome.result, outcome.err)
	}
	if flaky.calls != 1 {
		t.Fatalf("deterministic calls = %d, want 1", flaky.calls)
	}
}

// TestCancelRunAbortsLiveRun is #831's core: an operator cancel of a live run
// interrupts its active attempt, unwinds the owner, and finalizes terminal
// phase aborted — recording a run_canceled note and driving FinalizeTerminal
// (worktree teardown + claim release) with phase aborted, all through the same
// activeRun handshake the stall watchdog uses.
func TestCancelRunAbortsLiveRun(t *testing.T) {
	flaky := &flakyDeterministic{failUntil: 100}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())
	machine := retryFixtureMachineWithBackoff(t, 3, 10*time.Second)

	var finalizedPhase journal.RunPhase
	var finalizeCalls int32
	r.cfg.FinalizeTerminal = func(runID string, phase journal.RunPhase) error {
		if runID == "cancel-live" {
			finalizedPhase = phase
			atomic.AddInt32(&finalizeCalls, 1)
		}
		return nil
	}

	type startOutcome struct {
		result Result
		err    error
	}

	done := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   "cancel-live",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- startOutcome{result: result, err: err}
	}()

	runDir := filepath.Join(runsDir, "cancel-live")
	waitForRunEvent(t, runDir, "run to enter retry backoff", func(event journal.Event) bool {
		return event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error"
	})

	start := time.Now()
	result, cancelled, err := r.CancelRun("cancel-live", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled || result.Phase != journal.PhaseAborted {
		t.Fatalf("cancelled=%v result=%+v, want aborted", cancelled, result)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancel took %s to stop the run", elapsed)
	}

	outcome := <-done
	if outcome.err != nil || outcome.result.Phase != journal.PhaseAborted {
		t.Fatalf("Start() = %+v, %v, want aborted", outcome.result, outcome.err)
	}
	if atomic.LoadInt32(&finalizeCalls) == 0 || finalizedPhase != journal.PhaseAborted {
		t.Fatalf("FinalizeTerminal calls=%d phase=%s, want aborted teardown", finalizeCalls, finalizedPhase)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 ||
		events[len(events)-2].Error == nil ||
		events[len(events)-2].Error.Code != RunCanceledErrorCode ||
		events[len(events)-1].Type != journal.EventRunFinished ||
		events[len(events)-1].Status != string(journal.PhaseAborted) {
		t.Fatalf("terminal events = %+v, want run_canceled + run.finished(aborted)", events)
	}
}

// TestInterruptStageEscalatesLiveRun is #1995's core: an operator interrupt of
// a single running stage stops its active attempt and finalizes the run
// escalated (recoverable) rather than aborted, recording an operator-attributed
// stage_interrupted note and driving FinalizeTerminal with phase escalated —
// all through the same activeRun handshake CancelRun and the stall watchdog
// use. A stale target (a stage the run has already left) is refused rather than
// interrupting whatever is running now.
func TestInterruptStageEscalatesLiveRun(t *testing.T) {
	flaky := &flakyDeterministic{failUntil: 100}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())
	machine := retryFixtureMachineWithBackoff(t, 3, 10*time.Second)

	var finalizedPhase journal.RunPhase
	var finalizeCalls int32
	r.cfg.FinalizeTerminal = func(runID string, phase journal.RunPhase) error {
		if runID == "interrupt-live" {
			finalizedPhase = phase
			atomic.AddInt32(&finalizeCalls, 1)
		}
		return nil
	}

	type startOutcome struct {
		result Result
		err    error
	}

	done := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   "interrupt-live",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- startOutcome{result: result, err: err}
	}()

	runDir := filepath.Join(runsDir, "interrupt-live")
	waitForRunEvent(t, runDir, "run to enter retry backoff", func(event journal.Event) bool {
		return event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error"
	})

	// A stale target — a stage the run is not currently at — is refused, and
	// leaves the run running rather than interrupting the real stage.
	if _, escalated, err := r.InterruptStage("interrupt-live", "review", "operator@example", time.Now()); err == nil || escalated {
		t.Fatalf("stale-target interrupt = escalated %v, err %v, want refusal", escalated, err)
	}

	const actor = "operator@example"
	start := time.Now()
	result, escalated, err := r.InterruptStage("interrupt-live", "implement", actor, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !escalated || result.Phase != journal.PhaseEscalated {
		t.Fatalf("escalated=%v result=%+v, want escalated", escalated, result)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("interrupt took %s to stop the run", elapsed)
	}

	outcome := <-done
	if outcome.err != nil || outcome.result.Phase != journal.PhaseEscalated {
		t.Fatalf("Start() = %+v, %v, want escalated", outcome.result, outcome.err)
	}
	if atomic.LoadInt32(&finalizeCalls) == 0 || finalizedPhase != journal.PhaseEscalated {
		t.Fatalf("FinalizeTerminal calls=%d phase=%s, want escalated teardown", finalizeCalls, finalizedPhase)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	note := events[len(events)-2]
	if len(events) < 2 ||
		note.Error == nil ||
		note.Error.Code != StageInterruptedErrorCode ||
		note.Stage != "implement" ||
		note.Actor != actor ||
		events[len(events)-1].Type != journal.EventRunFinished ||
		events[len(events)-1].Status != string(journal.PhaseEscalated) {
		t.Fatalf("terminal events = %+v, want stage_interrupted(implement, %s) + run.finished(escalated)", events, actor)
	}
}

func TestHardStopRunLeavesCheckpointResumable(t *testing.T) {
	flaky := &flakyDeterministic{failUntil: 1}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())
	machine := retryFixtureMachineWithBackoff(t, 3, 10*time.Second)

	type startOutcome struct {
		result Result
		err    error
	}

	done := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   "hard-stop-live",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- startOutcome{result: result, err: err}
	}()

	runDir := filepath.Join(runsDir, "hard-stop-live")
	waitForRunEvent(t, runDir, "run to enter retry backoff", func(event journal.Event) bool {
		return event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error"
	})
	if !r.HardStopRun("hard-stop-live") {
		t.Fatal("HardStopRun did not find live run")
	}
	outcome := <-done
	if outcome.err != nil || outcome.result.Phase != journal.PhaseRunning {
		t.Fatalf("hard-stopped Start() = %+v, %v, want running checkpoint", outcome.result, outcome.err)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if phase, err := reader.Phase(); err != nil || phase != journal.PhaseRunning {
		t.Fatalf("phase after hard stop = %s, %v", phase, err)
	}

	resumed, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "hard-stop-live",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil || resumed.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume() = %+v, %v, want completed", resumed, err)
	}
}

func TestHardStopRunWhenStartedStopsLateActiveRegistration(t *testing.T) {
	r, _ := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return &flakyDeterministic{}, nil
	}, gate.NewAutomatedEvaluator())
	machine := retryFixtureMachineWithBackoff(t, 1, time.Second)
	r.HardStopRunWhenStarted("late-hard-stop")

	result, err := r.Start(context.Background(), StartInput{
		RunID:   "late-hard-stop",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil || result.Phase != journal.PhaseRunning {
		t.Fatalf("queued hard-stop Start() = %+v, %v, want running checkpoint", result, err)
	}
}

func TestExpireRunAbortsLiveRunDespiteActivity(t *testing.T) {
	flaky := &flakyDeterministic{failUntil: 100}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())
	machine := retryFixtureMachineWithBackoff(t, 3, 10*time.Second)

	type startOutcome struct {
		result Result
		err    error
	}
	done := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   "duration-live",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- startOutcome{result: result, err: err}
	}()

	runDir := filepath.Join(runsDir, "duration-live")
	waitForRunEvent(t, runDir, "run to enter retry backoff", func(event journal.Event) bool {
		return event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error"
	})
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	result, expired, err := r.ExpireRun("duration-live", identity.StartedAt.Add(2*time.Hour), identity.StartedAt, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !expired || result.Phase != journal.PhaseAborted {
		t.Fatalf("expired=%v result=%+v, want aborted", expired, result)
	}
	outcome := <-done
	if outcome.err != nil || outcome.result.Phase != journal.PhaseAborted {
		t.Fatalf("Start() = %+v, %v, want aborted", outcome.result, outcome.err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-2].Error == nil ||
		events[len(events)-2].Error.Code != RunDurationExceededErrorCode {
		t.Fatalf("terminal events = %+v, want duration-exceeded + run.finished(aborted)", events)
	}
}

// TestCancelRunReportsNoLiveOwner covers the daemon-sweep discriminator: a
// running run this Runner does not actively own is not cancelled here (the
// caller reports "not currently running by this daemon" rather than editing the
// journal behind a would-be owner's back).
func TestCancelRunReportsNoLiveOwner(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "unowned-run", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("implement")
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	var finalized bool
	r, err := New(Config{
		Worktrees: manager,
		RunsDir:   runsDir,
		FinalizeTerminal: func(string, journal.RunPhase) error {
			finalized = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, cancelled, err := r.CancelRun("unowned-run", now)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled || finalized {
		t.Fatalf("cancelled=%v finalized=%v, want no-op for an unowned run", cancelled, finalized)
	}
	if result.Phase != "" {
		t.Fatalf("result.Phase = %q, want empty (no live owner)", result.Phase)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, "unowned-run"))
	if err != nil {
		t.Fatal(err)
	}
	if phase, err := reader.Phase(); err != nil || phase != journal.PhaseRunning {
		t.Fatalf("phase=%s err=%v, want still running", phase, err)
	}
}

func TestEscalateStalledInterruptsPostStageHandler(t *testing.T) {
	const runID = "stalled-handler"
	r, _ := newTestRunner(t, map[string]stubTaskResult{
		runID + ":implement": {status: apiv1.ResultBlocked, summary: "waiting on dependency"},
	}, gate.NewAutomatedEvaluator())
	r.stalledCancelGrace = 20 * time.Millisecond
	r.stalledTerminalGrace = time.Second
	handlerStarted := make(chan struct{})
	prepareStarted := make(chan struct{})
	releasePrepare := make(chan struct{})
	r.cfg.Blocked = func(ctx context.Context, _ BlockedOutcome) error {
		close(handlerStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	var prepareCalls int
	r.cfg.PrepareTerminal = func(string, journal.RunPhase, *journal.Run) error {
		prepareCalls++
		close(prepareStarted)
		<-releasePrepare
		return nil
	}

	machine := fixtureMachine(t)

	type startOutcome struct {
		result Result
		err    error
	}
	done := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   runID,
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub,
				Owner:    "acme",
				Name:     "web",
				Branch:   "main",
			},
		})
		done <- startOutcome{result: result, err: err}
	}()

	select {
	case <-handlerStarted:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("run did not enter blocked handler")
	}

	type escalationOutcome struct {
		result    Result
		escalated bool
		err       error
	}
	escalationDone := make(chan escalationOutcome, 1)
	go func() {
		result, escalated, err := r.EscalateStalled(runID, time.Now().Add(2*time.Hour), 45*time.Minute)
		escalationDone <- escalationOutcome{result: result, escalated: escalated, err: err}
	}()
	select {
	case <-prepareStarted:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("stalled takeover did not enter terminal preparation")
	}
	close(releasePrepare)
	escalation := <-escalationDone
	if escalation.err != nil {
		t.Fatal(escalation.err)
	}
	result, escalated := escalation.result, escalation.escalated
	if !escalated || result.Phase != journal.PhaseEscalated {
		t.Fatalf("escalated=%v result=%+v", escalated, result)
	}
	outcome := <-done
	if outcome.err != nil || outcome.result.Phase != journal.PhaseEscalated {
		t.Fatalf("Start() = %+v, %v", outcome.result, outcome.err)
	}
	if prepareCalls != 1 {
		t.Fatalf("terminal preparation calls = %d, want 1", prepareCalls)
	}
}

func TestEscalateStalledDoesNotTakeOverNormalTerminalPreparation(t *testing.T) {
	const runID = "normal-terminal-preparation"
	r, runsDir := newTestRunner(t, map[string]stubTaskResult{
		runID + ":implement": {status: apiv1.ResultSuccess},
	}, gate.NewAutomatedEvaluator())
	r.stalledCancelGrace = 20 * time.Millisecond
	r.stalledTerminalGrace = time.Second

	prepareStarted := make(chan struct{})
	releasePrepare := make(chan struct{})
	var prepareOnce sync.Once
	var releaseOnce sync.Once
	var prepareCalls atomic.Int32
	release := func() { releaseOnce.Do(func() { close(releasePrepare) }) }
	defer release()
	r.cfg.PrepareTerminal = func(string, journal.RunPhase, *journal.Run) error {
		prepareCalls.Add(1)
		prepareOnce.Do(func() { close(prepareStarted) })
		<-releasePrepare
		return nil
	}

	type startOutcome struct {
		result Result
		err    error
	}
	machine := fixtureMachine(t)
	startDone := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   runID,
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub,
				Owner:    "acme",
				Name:     "web",
				Branch:   "main",
			},
		})
		startDone <- startOutcome{result: result, err: err}
	}()

	select {
	case <-prepareStarted:
	case <-time.After(stalledWaitTimeout()):
		t.Fatal("run did not enter normal terminal preparation")
	}

	type escalationOutcome struct {
		result    Result
		escalated bool
		err       error
	}
	escalationDone := make(chan escalationOutcome, 1)
	go func() {
		result, escalated, err := r.EscalateStalled(runID, time.Now().Add(2*time.Hour), 45*time.Minute)
		escalationDone <- escalationOutcome{result: result, escalated: escalated, err: err}
	}()

	select {
	case escalation := <-escalationDone:
		t.Fatalf("EscalateStalled returned before normal terminal preparation completed: %+v", escalation)
	case <-time.After(4 * r.stalledCancelGrace):
	}
	if got := prepareCalls.Load(); got != 1 {
		t.Fatalf("terminal preparation calls before release = %d, want 1", got)
	}
	release()

	started := <-startDone
	if started.err != nil || started.result.Phase != journal.PhaseCompleted {
		t.Fatalf("Start() = %+v, %v", started.result, started.err)
	}
	escalation := <-escalationDone
	if escalation.err != nil || escalation.escalated || escalation.result.Phase != journal.PhaseCompleted {
		t.Fatalf("EscalateStalled() = %+v, escalated=%v, err=%v", escalation.result, escalation.escalated, escalation.err)
	}

	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var finished int
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			finished++
			if event.Status != string(journal.PhaseCompleted) {
				t.Fatalf("run.finished status = %q, want completed", event.Status)
			}
		}
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == RunStalledErrorCode {
			t.Fatalf("normal terminal path was also escalated: %+v", event)
		}
	}
	if finished != 1 {
		t.Fatalf("run.finished events = %d, want 1", finished)
	}
}

func TestEscalateStalledTakesOverWedgedOwnerAfterIdleHeartbeatTicks(t *testing.T) {
	wedged := &wedgedDeterministic{started: make(chan struct{})}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return wedged, nil
	}, gate.NewAutomatedEvaluator())
	r.stalledCancelGrace = 20 * time.Millisecond
	ticker := &fakeHeartbeatTicker{
		ticks:   make(chan time.Time),
		stopped: make(chan struct{}),
	}
	r.newHeartbeatTicker = func(time.Duration) heartbeatTicker { return ticker }

	machine := fixtureMachine(t)
	type startOutcome struct {
		result Result
		err    error
	}
	done := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   "wedged-owner",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub,
				Owner:    "acme",
				Name:     "web",
				Branch:   "main",
			},
		})
		done <- startOutcome{result: result, err: err}
	}()

	select {
	case <-wedged.started:
	case <-time.After(stalledWaitTimeout()):
		t.Fatal("wedged executor did not start")
	}
	for i := 0; i < 2; i++ {
		select {
		case ticker.ticks <- time.Now():
		case <-time.After(runnerTestWaitTimeout):
			t.Fatal("heartbeat goroutine did not receive idle tick")
		}
	}

	runDir := filepath.Join(runsDir, "wedged-owner")
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventStageHeartbeat {
			t.Fatalf("idle ticker masked wedged executor with heartbeat: %+v", event)
		}
	}

	result, escalated, err := r.EscalateStalled("wedged-owner", time.Now().Add(2*time.Hour), 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !escalated || result.Phase != journal.PhaseEscalated {
		t.Fatalf("escalated=%v result=%+v", escalated, result)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.result.Phase != journal.PhaseEscalated {
			t.Fatalf("Start() = %+v, %v", outcome.result, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after watchdog takeover")
	}
}

func TestEscalateStalledPreservesProgressingAgenticGateBeforeHeartbeatFlush(t *testing.T) {
	reviewer := &progressingGateReviewer{
		started:  make(chan struct{}),
		progress: make(chan struct{}),
		reported: make(chan struct{}),
		release:  make(chan struct{}),
	}
	released := false
	defer func() {
		if !released {
			close(reviewer.release)
		}
	}()
	runID := "progressing-gate"
	machine := agenticGateMachine(t)
	r, _ := newAgenticGateRunner(t, map[string]stubTaskResult{
		runID + ":implement": {status: apiv1.ResultSuccess},
	}, reviewer, nil)
	taskTicker := &fakeHeartbeatTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
	gateTicker := &fakeHeartbeatTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
	tickerCalls := 0
	r.newHeartbeatTicker = func(time.Duration) heartbeatTicker {
		tickerCalls++
		if tickerCalls == 1 {
			return taskTicker
		}
		return gateTicker
	}

	type startOutcome struct {
		result Result
		err    error
	}
	done := make(chan startOutcome, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID:   runID,
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub,
				Owner:    "acme",
				Name:     "web",
				Branch:   "main",
			},
		})
		done <- startOutcome{result: result, err: err}
	}()

	select {
	case <-reviewer.started:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("agentic reviewer did not start")
	}
	timeout := 20 * time.Millisecond
	select {
	case <-time.After(2 * timeout):
	case <-done:
		t.Fatal("run finished before reviewer progress")
	}
	reviewer.progress <- struct{}{}
	select {
	case <-reviewer.reported:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("agentic reviewer did not report progress")
	}

	result, escalated, err := r.EscalateStalled(runID, time.Now(), timeout)
	if err != nil {
		t.Fatal(err)
	}
	if escalated || result.Phase != journal.PhaseRunning {
		t.Fatalf("progressing gate escalated=%v result=%+v", escalated, result)
	}

	select {
	case gateTicker.ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("gate heartbeat goroutine did not receive tick")
	}
	waitForRunEvent(t, filepath.Join(r.cfg.RunsDir, runID), "agentic gate progress heartbeat", func(event journal.Event) bool {
		return event.Type == journal.EventStageHeartbeat && event.Stage == "review" && event.Attempt == 1
	})

	close(reviewer.release)
	released = true
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.result.Phase != journal.PhaseCompleted {
			t.Fatalf("Start() = %+v, %v", outcome.result, outcome.err)
		}
	case <-time.After(stalledWaitTimeout()):
		t.Fatal("run did not finish after reviewer release")
	}
}
