package runner

import (
	"context"
	"path/filepath"
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
	case <-time.After(5 * time.Second):
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
		case <-time.After(time.Second):
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
	case <-time.After(5 * time.Second):
		t.Fatal("agentic reviewer did not start")
	}
	timeout := 20 * time.Millisecond
	time.Sleep(2 * timeout)
	reviewer.progress <- struct{}{}
	select {
	case <-reviewer.reported:
	case <-time.After(time.Second):
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
