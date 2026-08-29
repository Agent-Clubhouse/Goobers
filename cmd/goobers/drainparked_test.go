package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// writeParkedFixtureRun writes a run journal left in a given phase. A run with
// no terminal event is exactly the shape of one parked at a gate: Start has
// returned, so the drain holds nothing for it, but it is not finished.
func writeParkedFixtureRun(t *testing.T, root, runID, workflow string, terminal journal.RunPhase) {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:     runID,
		Workflow:  workflow,
		StartedAt: time.Now().Add(-2 * time.Minute),
	}, nil)
	if err != nil {
		t.Fatalf("create fixture run %s: %v", runID, err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "review", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if terminal != "" {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(terminal)}); err != nil {
			t.Fatalf("append run.finished: %v", err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close fixture run %s: %v", runID, err)
	}
}

// #3453: Start returns when a run pauses at a gate, releasing both the
// WaitGroup and the registry entry -- so ActiveRuns() cannot see it and the
// drain printed "no in-flight runs remain" while it sat there non-terminal.
func TestParkedNonTerminalRunsFindsGatePausedRun(t *testing.T) {
	root := initDemo(t)
	writeParkedFixtureRun(t, root, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "implementation", "")

	parked := parkedNonTerminalRuns(instance.NewLayout(root), nil)
	if len(parked) != 1 {
		t.Fatalf("parked = %+v, want exactly the one gate-paused run", parked)
	}
	if parked[0].RunID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || parked[0].Workflow != "implementation" {
		t.Fatalf("parked[0] = %+v, want the fixture run named", parked[0])
	}
}

// A terminal run is finished, not parked. Reporting it would train operators to
// ignore the warning, which is worse than not printing one.
func TestParkedNonTerminalRunsIgnoresTerminalRuns(t *testing.T) {
	root := initDemo(t)
	writeParkedFixtureRun(t, root, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "implementation", journal.PhaseCompleted)
	writeParkedFixtureRun(t, root, "cccccccccccccccccccccccccccccccc", "implementation", journal.PhaseFailed)

	if parked := parkedNonTerminalRuns(instance.NewLayout(root), nil); len(parked) != 0 {
		t.Fatalf("parked = %+v, want none: completed and failed runs are not parked", parked)
	}
}

// A run the drain IS holding is already reported by the existing progress line.
// Listing it twice, under two different descriptions, would misstate how many
// runs are at risk.
func TestParkedNonTerminalRunsExcludesHeldRuns(t *testing.T) {
	root := initDemo(t)
	writeParkedFixtureRun(t, root, "dddddddddddddddddddddddddddddddd", "implementation", "")

	active := []trackedRun{{RunID: "dddddddddddddddddddddddddddddddd", Workflow: "implementation"}}
	if parked := parkedNonTerminalRuns(instance.NewLayout(root), active); len(parked) != 0 {
		t.Fatalf("parked = %+v, want none: the run is held by the drain and already reported", parked)
	}
}

// The whole point: the drain must stop claiming nothing remains while a
// non-terminal run exists that a restart will leave for the next boot.
func TestDrainReportsParkedRunsAlongsideNoInFlightMessage(t *testing.T) {
	var stdout strings.Builder
	registry := &daemonRunnerRegistry{}
	release := make(chan struct{})
	close(release)

	result := drainDaemonRuns(&sync.WaitGroup{}, func() { <-release }, registry, 0, nil, &stdout,
		func([]trackedRun) []parkedRun {
			return []parkedRun{{RunID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Workflow: "implementation"}}
		})
	if result.forced {
		t.Fatalf("drain reported forced = true, want a clean drain")
	}
	out := stdout.String()
	if !strings.Contains(out, "no in-flight runs remain") {
		t.Fatalf("output = %q, want the existing held-run message preserved", out)
	}
	if !strings.Contains(out, "parked at a gate and NOT held by this drain") {
		t.Fatalf("output = %q, want parked runs surfaced", out)
	}
	if !strings.Contains(out, "implementation/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee") {
		t.Fatalf("output = %q, want the parked run named rather than counted", out)
	}
}

// Nil disables the report entirely, so every existing caller and test keeps
// its current output.
func TestDrainWithoutParkedReporterIsUnchanged(t *testing.T) {
	var stdout strings.Builder
	registry := &daemonRunnerRegistry{}
	release := make(chan struct{})
	close(release)

	drainDaemonRuns(&sync.WaitGroup{}, func() { <-release }, registry, 0, nil, &stdout, nil)
	if strings.Contains(stdout.String(), "parked at a gate") {
		t.Fatalf("output = %q, want no parked report when the reporter is nil", stdout.String())
	}
}
