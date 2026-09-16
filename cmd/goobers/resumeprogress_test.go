package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// #5199: the crash-resume phase ran for 51 minutes on the live instance and
// printed nothing between its start and done lines, so an operator could not
// tell a slow pass from a hung one. Every progress line has to carry the
// position and the tallies.
func TestResumeOutcomeSummaryNamesPositionAndTallies(t *testing.T) {
	outcome := resumeOutcome{
		Total:      1481,
		Examined:   412,
		Resumed:    []string{"a"},
		Warned:     []string{"b", "c"},
		Reattached: []string{"d"},
		Terminal:   make([]terminalFinalization, 400),
	}
	summary := outcome.Summary()
	for _, want := range []string{"examined=412/1481", "resumed=1", "reattached=1", "terminal=400", "skipped=2"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("Summary() = %q, missing %q", summary, want)
		}
	}
}

// The reporter throttles stdout but never the phase tracker: the readiness
// diagnostic must name the current position even between log lines.
func TestStartupProgressReporterThrottlesOutputNotTracker(t *testing.T) {
	var out bytes.Buffer
	tracker := &startupPhaseTracker{}
	tracker.set("crash-resume", "candidates=3")
	reporter := newStartupProgressReporter(&out, tracker, "crash-resume", "candidates=3", time.Hour)

	reporter.report("examined=1/3")
	reporter.report("examined=2/3")

	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing inside the throttle interval", out.String())
	}
	if _, target, _ := tracker.snapshot(); !strings.Contains(target, "examined=2/3") {
		t.Fatalf("tracker target = %q, want the latest position", target)
	}

	reporter.interval = 0
	reporter.report("examined=3/3")
	if !strings.Contains(out.String(), "phase=crash-resume status=progress") || !strings.Contains(out.String(), "examined=3/3") {
		t.Fatalf("stdout = %q, want a progress line naming the position", out.String())
	}
}

// A progress report for a phase that has already moved on must not overwrite
// the tracker's current phase target — the readiness diagnostic would then
// name the wrong phase's position.
func TestStartupProgressReporterDoesNotClobberLaterPhase(t *testing.T) {
	var out bytes.Buffer
	tracker := &startupPhaseTracker{}
	reporter := newStartupProgressReporter(&out, tracker, "crash-resume", "candidates=3", time.Hour)
	tracker.set("ready", "api")

	reporter.report("examined=1/3")

	if phase, target, _ := tracker.snapshot(); phase != "ready" || target != "api" {
		t.Fatalf("tracker = %q/%q, want the later phase untouched", phase, target)
	}
}

func TestStartupInventoryCountsNameEachSource(t *testing.T) {
	counts := startupInventoryCounts{Marked: 17, Discovered: 5, More: true}
	for _, want := range []string{"marked=17", "discovered=5", "candidates=22", "more=true"} {
		if !strings.Contains(counts.String(), want) {
			t.Fatalf("String() = %q, missing %q", counts.String(), want)
		}
	}
}

// startStartupTerminalFinalize must not leave the shutdown join waiting when
// there is nothing to finalize.
func TestStartStartupTerminalFinalizeClosesWithNoCandidates(t *testing.T) {
	finalizer := startStartupTerminalFinalize(context.Background(), &schedulerSetup{}, nil)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		finalizer.finishAfterDrain()
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("empty terminal finalization never finished")
	}
}

// Deferring terminal cleanup past readiness must not turn into dropping it
// when shutdown arrives first: whatever the background pass did not reach
// gets one bounded turn after drain (#5199).
func TestStartupTerminalFinalizerFinishesWhatCancellationLeft(t *testing.T) {
	t.Chdir(t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var finalized []string
	finalizer := &startupTerminalFinalizer{
		remaining: []terminalFinalization{{identity: journal.RunIdentity{RunID: "0123456789abcdef0123456789abcdef"}}},
		reporter:  newSweepErrorReporter(nil, "startup_terminal_finalize_failed"),
		done:      make(chan struct{}),
	}
	close(finalizer.done)

	// The cancelled background pass takes nothing off the queue.
	if err := finalizer.run(ctx, func(done, _ int) { finalized = append(finalized, "background") }); err != nil {
		t.Fatalf("cancelled pass reported %v, want no failure", err)
	}
	if len(finalized) != 0 || len(finalizer.remaining) != 1 {
		t.Fatalf("cancelled pass consumed the queue: finalized=%v remaining=%d", finalized, len(finalizer.remaining))
	}

	// The post-drain turn runs on a context of its own, so the candidate is
	// finalized rather than abandoned. It fails (no such run), which is the
	// reporter's business, not this assertion's.
	finalizer.finishAfterDrain()
	if len(finalizer.remaining) != 0 {
		t.Fatalf("post-drain turn left %d candidates unfinalized", len(finalizer.remaining))
	}
}

// A SIGTERM during a large deferred pass must not hold the drain open for the
// whole pass. Cancellation is observed between candidates, so nothing is left
// half-finalized and the remaining candidates keep their active markers for
// the next start.
func TestStartupTerminalFinalizerStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A candidate that would certainly fail if it ran: no runner, and a
	// layout rooted at a path with no run of this ID in it.
	candidates := []terminalFinalization{{
		layout:   instance.NewLayout(t.TempDir()),
		runsDir:  t.TempDir(),
		identity: journal.RunIdentity{RunID: "0123456789abcdef0123456789abcdef"},
		phase:    journal.PhaseCompleted,
	}}

	var finalized int
	finalizer := &startupTerminalFinalizer{remaining: candidates}
	if err := finalizer.run(ctx, func(int, int) { finalized++ }); err != nil {
		t.Fatalf("cancelled pass reported %v, want no failure", err)
	}
	if finalized != 0 {
		t.Fatalf("finalized %d candidates after cancellation, want none", finalized)
	}
}
