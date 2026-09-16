package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
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
	done := startStartupTerminalFinalize(&schedulerSetup{}, nil, nil)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("empty terminal finalization never closed its done channel")
	}
}
