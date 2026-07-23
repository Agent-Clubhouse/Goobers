package runner

import (
	"context"
	"path/filepath"
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		reader, err := journal.OpenRead(runDir)
		if err == nil {
			events, readErr := reader.Events()
			if readErr != nil {
				t.Fatal(readErr)
			}
			found := false
			for _, event := range events {
				if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error" {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not enter retry backoff")
		}
		time.Sleep(10 * time.Millisecond)
	}

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
	deadline := time.Now().Add(5 * time.Second)
	for {
		reader, err := journal.OpenRead(runDir)
		if err == nil {
			events, readErr := reader.Events()
			if readErr != nil {
				t.Fatal(readErr)
			}
			found := false
			for _, event := range events {
				if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error" {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not enter retry backoff")
		}
		time.Sleep(10 * time.Millisecond)
	}

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
	r.cfg.Blocked = func(ctx context.Context, _ BlockedOutcome) error {
		close(handlerStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	var prepareCalls int
	r.cfg.PrepareTerminal = func(string, journal.RunPhase, *journal.Run) error {
		prepareCalls++
		time.Sleep(100 * time.Millisecond)
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

	result, escalated, err := r.EscalateStalled(runID, time.Now().Add(2*time.Hour), 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
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
	case <-time.After(5 * time.Second):
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

	time.Sleep(4 * r.stalledCancelGrace)
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
	case <-time.After(5 * time.Second):
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
	r := newAgenticGateRunner(t, map[string]stubTaskResult{
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
	time.Sleep(2 * timeout)
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
	deadline := time.Now().Add(time.Second)
	for {
		reader, err := journal.OpenRead(filepath.Join(r.cfg.RunsDir, runID))
		if err != nil {
			t.Fatal(err)
		}
		events, err := reader.Events()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			if event.Type == journal.EventStageHeartbeat && event.Stage == "review" && event.Attempt == 1 {
				found = true
				break
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agentic gate progress did not emit a heartbeat")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(reviewer.release)
	released = true
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.result.Phase != journal.PhaseCompleted {
			t.Fatalf("Start() = %+v, %v", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish after reviewer release")
	}
}
