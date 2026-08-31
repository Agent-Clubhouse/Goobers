package localscheduler

// Tests for checkpoint 3's scheduler half (#2860, dsl-3.0.md §5): an entry
// the boot-time constraint solve marked refused is journaled when the
// scheduler learns it, refused per-run with the named diagnostic on any
// trigger, and skipped by Tick — while every other entry keeps serving.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestPlacementRefusalJournaledAtConstruction: New records one
// workflow.refused per refused entry, so `goobers status` names the refusal
// without waiting for a dispatch attempt.
func TestPlacementRefusalJournaledAtConstruction(t *testing.T) {
	_, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:         "win-build",
			Gaggle:           "example",
			PlacementRefusal: `stage "build" requires os "windows"; no runner satisfies it`,
		},
		{Workflow: "healthy", Gaggle: "example"},
	})
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var refused []journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventWorkflowRefused {
			refused = append(refused, ev)
		}
	}
	if len(refused) != 1 {
		t.Fatalf("workflow.refused events = %d, want exactly one (only the refused entry): %+v", len(refused), events)
	}
	if refused[0].Workflow != "win-build" || refused[0].Gaggle != "example" ||
		!strings.Contains(refused[0].Reason, `requires os "windows"`) {
		t.Fatalf("refusal event must carry identity and the named diagnostic: %+v", refused[0])
	}
}

// TestPlacementRefusalRefusesDispatchAndOthersServe is the scheduler half of
// dsl-3.0.md §9 item 9: the refused workflow's trigger is rejected with the
// named diagnostic (journaled tick.skipped), and a sibling workflow on the
// same scheduler dispatches normally.
func TestPlacementRefusalRefusesDispatchAndOthersServe(t *testing.T) {
	refusedStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	healthyStarter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:         "win-build",
			Readiness:        apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:          refusedStarter,
			PlacementRefusal: `stage "build" requires os "windows"; no runner satisfies it: runner "self" provides os "linux" (stage requires "windows")`,
		},
		{
			Workflow:  "healthy",
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:   healthyStarter,
		},
	})

	_, err := sched.Trigger(context.Background(), "win-build", time.Now())
	if err == nil {
		t.Fatal("expected the refused workflow's trigger to be rejected")
	}
	var rejected *TriggerRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected *TriggerRejectedError, got %T: %v", err, err)
	}
	if !strings.HasPrefix(rejected.Reason, ReasonPlacementUnsatisfiable) {
		t.Errorf("reason must carry the stable prefix %q: %q", ReasonPlacementUnsatisfiable, rejected.Reason)
	}
	if !strings.Contains(rejected.Reason, `requires os "windows"`) {
		t.Errorf("reason must carry the solver's named diagnostic: %q", rejected.Reason)
	}
	if rejected.Transient() {
		t.Error("a placement refusal is permanent for the pinned inventory (restart-only), never transient")
	}
	if refusedStarter.count() != 0 {
		t.Errorf("the refused workflow must not start, got %d run(s)", refusedStarter.count())
	}

	// The daemon-survives half: an unrelated workflow on the same scheduler
	// dispatches unchanged.
	if _, err := sched.Trigger(context.Background(), "healthy", time.Now()); err != nil {
		t.Fatalf("the healthy workflow must keep serving, got %v", err)
	}
	waitForCount(t, func() int { return healthyStarter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawSkip bool
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && strings.HasPrefix(ev.Reason, ReasonPlacementUnsatisfiable) {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Errorf("expected a tick.skipped event with the placement refusal: %+v", events)
	}
}

// TestPlacementRefusalSkippedByTick: a refused entry is skipped before any
// backlog/provider polling — no counter call, no per-tick journal noise
// (the refusal is already journaled once at construction).
func TestPlacementRefusalSkippedByTick(t *testing.T) {
	counter := &countingBacklogCounter{}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:         "win-build",
		Readiness:        apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:          &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}},
		BacklogCounter:   counter,
		PollProvider:     apiv1.ProviderGitHub,
		PlacementRefusal: "stage \"build\" requires os \"windows\"; no runner satisfies it",
	}})
	sched.Tick(context.Background(), time.Now())
	if counter.calls != 0 {
		t.Errorf("a refused workflow must not spend provider polls, got %d", counter.calls)
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	skips := 0
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped {
			skips++
		}
	}
	if skips != 0 {
		t.Errorf("Tick must skip a refused entry silently (journaled once at load), got %d tick.skipped", skips)
	}
}

// TestUnrefusedEntryTicksAndTriggers is the #3987 counterpart of the two
// tests above: the SAME entry shape with the refusal lifted ticks and
// dispatches on an explicit trigger.
//
// It exists because "stop stamping PlacementRefusal on engine-selected
// entries" is only a fix if an entry without the field actually serves. The
// two assertions are the two ways the outage manifested on the live instance
// (#3987): backlog-curation was skipped silently on every tick, and an
// explicit trigger came back ReasonPlacementUnsatisfiable.
func TestUnrefusedEntryTicksAndTriggers(t *testing.T) {
	unrefused := func(starter Starter, counter BacklogCounter) []WorkflowEntry {
		return []WorkflowEntry{{
			Workflow:       "pod-pinned",
			Gaggle:         "example",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:        starter,
			BacklogCounter: counter,
			PollProvider:   apiv1.ProviderGitHub,
		}}
	}

	counter := &countingBacklogCounter{}
	ticked, _ := newTestScheduler(t, unrefused(&fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}, counter))
	ticked.Tick(context.Background(), time.Now())
	if counter.calls == 0 {
		t.Error("an entry carrying no placement refusal must be polled by Tick, not skipped")
	}

	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	triggered, _ := newTestScheduler(t, unrefused(starter, nil))
	if _, err := triggered.Trigger(context.Background(), "pod-pinned", time.Now()); err != nil {
		t.Fatalf("an entry carrying no placement refusal must accept an explicit trigger, got %v", err)
	}
	waitForCount(t, func() int { return starter.count() }, 1)
}

type countingBacklogCounter struct{ calls int }

func (c *countingBacklogCounter) EligibleCount(context.Context) (int, error) {
	c.calls++
	return 1, nil
}
