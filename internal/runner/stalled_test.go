package runner

import (
	"path/filepath"
	"testing"
	"time"

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
