package localscheduler

// Tests for #5277: a scheduled workflow that keeps firing on time but is
// refused admission every single time is wedged, not busy — and until now the
// two were indistinguishable. #1868's check asks whether the trigger loop is
// still evaluating; for this shape it is, so it stays silent.
//
// This is the shape that starved the cloud instance in #5272: backlog-curation
// at maxConcurrentRuns: 1, its one slot held by a run wedged in `running`, so
// every tick fired, was refused max-parallel, and skipped — for most of a day,
// with the failure rate at 0% and every health signal green.

import (
	"context"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestWedgedConcurrencyOneLaneJournalsStarvation is #5272 in miniature, driven
// through Tick: one blocked run holds the only slot, and the lane keeps firing
// into a refusal until the alarm fires.
func TestWedgedConcurrencyOneLaneJournalsStarvation(t *testing.T) {
	now := time.Date(2026, time.September, 17, 4, 0, 0, 0, time.UTC)
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}, block: make(chan struct{})}
	t.Cleanup(func() { close(starter.block) })

	entries := []WorkflowEntry{{
		Workflow:  "backlog-curation",
		Gaggle:    "goobers",
		Schedules: []Schedule{mustParseSchedule(t, "* * * * *")},
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}}
	scheduler, dir := newTestScheduler(t, entries)
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "backlog-curation"}

	// The first tick dispatches and the run never returns: the slot is now held.
	// Start runs on the dispatch goroutine, so wait for it to be observable
	// rather than reading the counter straight after Tick — the slot is not
	// actually held until Start has been entered, and asserting before that is
	// a race, not a test.
	setLastEval(scheduler, identity, now.Add(-time.Minute))
	scheduler.Tick(context.Background(), now)
	waitForStarts(t, starter, 1)
	if starved := starvedEvents(t, dir); len(starved) != 0 {
		t.Fatalf("a lane that just dispatched must not be starved yet: %+v", starved)
	}

	// Every later tick fires on schedule and is refused for the held slot.
	for i := 1; i <= triggerStallMultiple+1; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		setLastEval(scheduler, identity, at.Add(-time.Minute))
		scheduler.Tick(context.Background(), at)
	}

	starved := starvedEvents(t, dir)
	if len(starved) != 1 {
		t.Fatalf("workflow.starved events = %d, want exactly one: %+v", len(starved), starved)
	}
	if starved[0].Workflow != "backlog-curation" || starved[0].Gaggle != "goobers" {
		t.Errorf("starvation event must carry the workflow identity: %+v", starved[0])
	}
	if !strings.Contains(starved[0].Reason, "dispatched no run") {
		t.Errorf("reason must say the trigger fired but nothing dispatched: %q", starved[0].Reason)
	}
	if !strings.Contains(starved[0].Reason, ReasonMaxParallel) {
		t.Errorf("reason must name the refusal an operator has to act on: %q", starved[0].Reason)
	}
}

// A busy lane is not a wedged lane: refusals that keep clearing must never
// accumulate into an alarm, or the signal is worthless on a healthy instance.
func TestBusyLaneThatKeepsDispatchingIsNotStarved(t *testing.T) {
	now := time.Date(2026, time.September, 17, 4, 0, 0, 0, time.UTC)
	entries := []WorkflowEntry{{
		Workflow:  "implementation",
		Gaggle:    "goobers",
		Schedules: []Schedule{mustParseSchedule(t, "* * * * *")},
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
	}}
	scheduler, dir := newTestScheduler(t, entries)
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "implementation"}

	// Refused for most of the window, then admitted — the shape of a lane whose
	// runs are simply taking a while.
	for i := 0; i < triggerStallMultiple-1; i++ {
		scheduler.recordDispatchOutcome(identity, false, ReasonMaxParallel, now.Add(time.Duration(i)*time.Minute))
	}
	scheduler.recordDispatchOutcome(identity, true, "", now.Add(triggerStallMultiple*time.Minute))
	scheduler.journalCapacityStarvation(entries, now.Add(10*triggerStallMultiple*time.Minute))

	if starved := starvedEvents(t, dir); len(starved) != 0 {
		t.Fatalf("a lane that dispatched within the window must not be starved: %+v", starved)
	}
}

// A workflow an operator deliberately stopped is behaving as configured. Budget
// and quota refusals are not promises that capacity is about to exist, so they
// must never open a starvation window.
func TestDeliberatelyStoppedWorkflowIsNotStarved(t *testing.T) {
	now := time.Date(2026, time.September, 17, 4, 0, 0, 0, time.UTC)
	entries := []WorkflowEntry{{
		Workflow:  "quality-sprint",
		Gaggle:    "goobers",
		Schedules: []Schedule{mustParseSchedule(t, "* * * * *")},
	}}
	scheduler, dir := newTestScheduler(t, entries)
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "quality-sprint"}

	for i := 0; i < 10*triggerStallMultiple; i++ {
		scheduler.recordDispatchOutcome(identity, false, ReasonBudget, now.Add(time.Duration(i)*time.Minute))
	}
	scheduler.journalCapacityStarvation(entries, now.Add(100*triggerStallMultiple*time.Minute))

	if starved := starvedEvents(t, dir); len(starved) != 0 {
		t.Fatalf("a deliberately stopped workflow must not alarm: %+v", starved)
	}
}

// One event per episode, like #1868: a chronically wedged lane must not flood
// the journal, and must become reportable again after recovering and re-wedging.
func TestCapacityStarvationReportedOncePerEpisode(t *testing.T) {
	now := time.Date(2026, time.September, 17, 4, 0, 0, 0, time.UTC)
	entries := []WorkflowEntry{{
		Workflow:  "backlog-curation",
		Gaggle:    "goobers",
		Schedules: []Schedule{mustParseSchedule(t, "* * * * *")},
	}}
	scheduler, dir := newTestScheduler(t, entries)
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "backlog-curation"}

	scheduler.recordDispatchOutcome(identity, false, ReasonMaxParallel, now)
	overdue := now.Add((triggerStallMultiple + 1) * time.Minute)
	for i := 0; i < 5; i++ {
		scheduler.journalCapacityStarvation(entries, overdue)
	}
	if starved := starvedEvents(t, dir); len(starved) != 1 {
		t.Fatalf("workflow.starved events = %d, want one per episode: %+v", len(starved), starved)
	}

	// Recovered, then wedged again: a new episode is reportable.
	scheduler.recordDispatchOutcome(identity, true, "", overdue)
	scheduler.recordDispatchOutcome(identity, false, ReasonMaxParallel, overdue)
	scheduler.journalCapacityStarvation(entries, overdue.Add((triggerStallMultiple+1)*time.Minute))
	if starved := starvedEvents(t, dir); len(starved) != 2 {
		t.Fatalf("workflow.starved events = %d, want a second episode reported: %+v", len(starved), starved)
	}
}

// A workflow with no schedule has no interval to measure against, so it is out
// of scope for this alarm however long it is refused.
func TestUnscheduledWorkflowIsNeverCapacityStarved(t *testing.T) {
	now := time.Date(2026, time.September, 17, 4, 0, 0, 0, time.UTC)
	entries := []WorkflowEntry{{
		Workflow: "merge-review",
		Gaggle:   "goobers",
	}}
	scheduler, dir := newTestScheduler(t, entries)
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "merge-review"}

	scheduler.recordDispatchOutcome(identity, false, ReasonMaxParallel, now)
	scheduler.journalCapacityStarvation(entries, now.Add(100*time.Hour))
	if starved := starvedEvents(t, dir); len(starved) != 0 {
		t.Fatalf("an unscheduled workflow has no interval to be overdue against: %+v", starved)
	}
}

// waitForStarts blocks until the starter has been entered want times. It bounds
// the wait so a genuine failure reports rather than hanging the suite; it never
// asserts how long the dispatch took, which would be a wall-clock assertion in
// CI (and a flake).
func waitForStarts(t *testing.T, starter *fakeStarter, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for starter.count() < want {
		if time.Now().After(deadline) {
			t.Fatalf("dispatch count = %d, want %d", starter.count(), want)
		}
		time.Sleep(time.Millisecond)
	}
}
