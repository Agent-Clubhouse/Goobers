package localscheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestDispatchRefusesUnmetCapability is RRQ-1/#1101's schedule-time fail-closed:
// a workflow whose RequiredCapabilities the runner does not claim is refused at
// schedule, with a diagnostic naming the missing capability — never scheduled
// to fail at run.
func TestDispatchRefusesUnmetCapability(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:             "implement",
		Readiness:            apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:              starter,
		RequiredCapabilities: []string{"dotnet@10", "xcode"},
	}}, WithRunnerCapabilities([]string{"dotnet@8"})) // claims neither

	_, err := sched.Trigger(context.Background(), "implement", time.Now())
	if err == nil {
		t.Fatal("expected the trigger to be refused, got nil error")
	}
	var rejected *TriggerRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected *TriggerRejectedError, got %T: %v", err, err)
	}
	// The diagnostic must name the missing capabilities and be non-transient
	// (a static runner claim never becomes satisfiable by waiting).
	if !strings.Contains(rejected.Reason, "dotnet@10") || !strings.Contains(rejected.Reason, "xcode") {
		t.Errorf("reason must name the missing capabilities: %q", rejected.Reason)
	}
	if !strings.HasPrefix(rejected.Reason, ReasonMissingCapability) {
		t.Errorf("reason must carry the stable prefix %q: %q", ReasonMissingCapability, rejected.Reason)
	}
	if rejected.Transient() {
		t.Error("a missing-capability refusal must not be transient")
	}

	// No run started, and the skip is journaled for a status reader.
	if starter.count() != 0 {
		t.Errorf("no run should have started, got %d", starter.count())
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawSkip bool
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && strings.HasPrefix(ev.Reason, ReasonMissingCapability) {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Errorf("expected a tick.skipped event with a missing-capability reason: %+v", events)
	}
}

// TestDispatchAdmitsMetCapability is the positive half: a run whose requirement
// the runner claims schedules unchanged.
func TestDispatchAdmitsMetCapability(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:             "implement",
		Readiness:            apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:              starter,
		RequiredCapabilities: []string{"dotnet@8"},
	}}, WithRunnerCapabilities([]string{"dotnet@8", "xcode"}))

	if _, err := sched.Trigger(context.Background(), "implement", time.Now()); err != nil {
		t.Fatalf("expected the met requirement to schedule, got %v", err)
	}
	waitForCount(t, func() int { return starter.count() }, 1)
}

// TestDispatchNoRequirementBehavesAsToday is the regression guard: a workflow
// that declares no RequiredCapabilities schedules even when the runner claims
// nothing — exactly as before RRQ-1.
func TestDispatchNoRequirementBehavesAsToday(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	// No WithRunnerCapabilities option at all (the common single-Go-gaggle case).
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})

	if _, err := sched.Trigger(context.Background(), "implement", time.Now()); err != nil {
		t.Fatalf("a no-requirement workflow must schedule unchanged, got %v", err)
	}
	waitForCount(t, func() int { return starter.count() }, 1)
}
