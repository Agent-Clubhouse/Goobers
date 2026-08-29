package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// TestWaitRendersFinalStageWhenRunCompletesDuringPoll covers #1557: the wait
// loop used to read the events file twice per poll — once to render progress,
// then again inside runPhase to test for terminality. The writer appends
// between those two reads, so the second could observe run.finished while the
// first had not yet seen it, and the loop returned without ever rendering the
// last stage's "finished" line.
//
// This guards the invariant — a terminal return must never leave the final
// events unrendered — by finishing the run from another goroutine while the
// loop polls.
//
// Be aware of what it does not do: it does NOT fail against the two-read
// implementation. Measured at 0 failures in 800 iterations pre-fix, because a
// same-process writer's appends are visible to the reader immediately, so both
// reads almost always agree. In production the journal is written by the daemon
// and read by the CLI, where cross-process visibility and scheduling delay make
// the two reads disagree far more readily. So this is an invariant guard
// against future regressions, not a reproduction of #1557.
func TestWaitRendersFinalStageWhenRunCompletesDuringPoll(t *testing.T) {
	oldPoll := runPollInterval
	runPollInterval = time.Millisecond
	t.Cleanup(func() { runPollInterval = oldPoll })

	const iterations = 50
	for i := range iterations {
		t.Run(fmt.Sprintf("iteration-%d", i), func(t *testing.T) {
			runsDir := t.TempDir()
			runID := fmt.Sprintf("run-%d", i)
			now := time.Now().UTC()

			run, err := journal.Create(runsDir, journal.RunIdentity{
				RunID:     runID,
				Workflow:  "demo",
				Gaggle:    "widgets",
				StartedAt: now,
				Trigger:   journal.Trigger{Kind: journal.TriggerManual},
			}, nil, journal.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatalf("create journal: %v", err)
			}
			if err := run.Append(journal.Event{
				Type: journal.EventStageStarted, Stage: "merge-preview", Attempt: 1, Time: now,
			}); err != nil {
				t.Fatalf("append stage.started: %v", err)
			}

			// Finish the run from another goroutine while the loop polls, so
			// the terminal records land in an unpredictable position relative
			// to the loop's reads.
			appended := make(chan error, 1)
			go func() {
				time.Sleep(time.Duration(i%7) * 300 * time.Microsecond) // Intentional jitter exercises concurrent drain ordering.
				if err := run.Append(journal.Event{
					Type: journal.EventStageFinished, Stage: "merge-preview", Attempt: 1,
					Status: "success", Time: now.Add(time.Second),
				}); err != nil {
					appended <- err
					return
				}
				err := run.Append(journal.Event{
					Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
					Time: now.Add(2 * time.Second),
				})
				appended <- err
			}()

			var progress synchronizedBuffer
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			phase, err := waitForRunTerminalWithReporter(
				ctx, runsDir, runID, newRunWaitReporter(runID, &progress))
			if err != nil {
				t.Fatalf("wait: %v", err)
			}
			if appendErr := <-appended; appendErr != nil {
				t.Fatalf("append terminal events: %v", appendErr)
			}
			if err := run.Close(); err != nil {
				t.Fatalf("close journal: %v", err)
			}
			if phase != journal.PhaseCompleted {
				t.Fatalf("phase = %s, want completed", phase)
			}

			// The run reached a terminal phase, so every event up to and
			// including the terminal one must already have been rendered.
			if got := progress.String(); !strings.Contains(got, "stage merge-preview finished") {
				t.Fatalf("wait returned %s without rendering the final stage line:\n%s", phase, got)
			}
		})
	}
}

// TestPhaseFromEventsMatchesReaderPhase pins the equivalence the wait loop now
// relies on: deriving the phase from an already-read slice must agree with
// Reader.Phase, including the resume-healing rule.
func TestPhaseFromEventsMatchesReaderPhase(t *testing.T) {
	runsDir := t.TempDir()
	now := time.Now().UTC()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:     "phase-equivalence",
		Workflow:  "demo",
		Gaggle:    "widgets",
		StartedAt: now,
		Trigger:   journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated), Time: now},
		{Type: journal.EventRunResumed, Status: string(journal.PhaseEscalated), Target: "implement", Time: now},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted), Time: now},
	} {
		if err := run.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	reader, err := journal.OpenRead(filepath.Join(runsDir, "phase-equivalence"))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	want, err := reader.Phase()
	if err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if got := journal.PhaseFromEvents(events); got != want {
		t.Fatalf("PhaseFromEvents = %s, Reader.Phase = %s", got, want)
	}
	if want != journal.PhaseCompleted {
		t.Fatalf("phase = %s, want completed after resume", want)
	}
}
