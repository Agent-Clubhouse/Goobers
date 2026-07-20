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
	handlerStarted := make(chan struct{})
	r.cfg.Blocked = func(ctx context.Context, _ BlockedOutcome) error {
		close(handlerStarted)
		<-ctx.Done()
		return ctx.Err()
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
}
