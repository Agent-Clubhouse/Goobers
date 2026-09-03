package localscheduler

// Tests for #1868: a scheduled workflow whose trigger goes silent — wedged,
// placement-refused, or auth-circuited — must journal a workflow.starved
// diagnostic instead of disappearing from the event stream with no signal at
// all, while a healthy lane firing on schedule stays quiet.

import (
	"context"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func mustParseSchedule(t *testing.T, expr string) Schedule {
	t.Helper()
	sched, err := ParseSchedule(expr)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", expr, err)
	}
	return sched
}

func starvedEvents(t *testing.T, dir string) []journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var starved []journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventWorkflowStarved {
			starved = append(starved, ev)
		}
	}
	return starved
}

func setLastEval(s *Scheduler, identity WorkflowIdentity, at time.Time) {
	s.mu.Lock()
	state := s.triggers[identity]
	state.LastEval = at
	s.triggers[identity] = state
	s.mu.Unlock()
}

// TestSilentScheduledTriggerJournalsStarvation: a lane the tick skips silently
// (placement refused) whose trigger has not fired for many schedule intervals
// is reported, naming the silence and the interval it exceeded.
func TestSilentScheduledTriggerJournalsStarvation(t *testing.T) {
	now := time.Date(2026, time.July, 29, 4, 0, 0, 0, time.UTC)
	scheduler, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:         "pr-remediation",
		Gaggle:           "goobers",
		Schedules:        []Schedule{mustParseSchedule(t, "* * * * *")},
		PlacementRefusal: "no runner satisfies the pinned inventory",
	}})
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "pr-remediation"}
	setLastEval(scheduler, identity, now.Add(-time.Hour))

	scheduler.Tick(context.Background(), now)

	starved := starvedEvents(t, dir)
	if len(starved) != 1 {
		t.Fatalf("workflow.starved events = %d, want exactly one: %+v", len(starved), starved)
	}
	if starved[0].Workflow != "pr-remediation" || starved[0].Gaggle != "goobers" {
		t.Errorf("starvation event must carry the workflow identity: %+v", starved[0])
	}
	if !strings.Contains(starved[0].Reason, "has not fired for 1h0m0s") ||
		!strings.Contains(starved[0].Reason, "1m0s schedule interval") {
		t.Errorf("reason must name the silence and the schedule interval: %q", starved[0].Reason)
	}
}

// TestFiringScheduledTriggerIsNotStarved: a lane that fires on this tick has a
// fresh baseline and must stay out of the diagnostic, even though its previous
// evaluation was long ago (a catch-up after a daemon outage).
func TestFiringScheduledTriggerIsNotStarved(t *testing.T) {
	now := time.Date(2026, time.July, 29, 4, 0, 0, 0, time.UTC)
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	scheduler, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implementation",
		Gaggle:    "goobers",
		Schedules: []Schedule{mustParseSchedule(t, "* * * * *")},
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "implementation"}
	setLastEval(scheduler, identity, now.Add(-time.Hour))

	scheduler.Tick(context.Background(), now)
	scheduler.Wait()

	if starter.count() != 1 {
		t.Fatalf("healthy workflow runs = %d, want the catch-up dispatch", starter.count())
	}
	if starved := starvedEvents(t, dir); len(starved) != 0 {
		t.Fatalf("a workflow that fired must not be reported starved: %+v", starved)
	}
}

// TestScheduledTriggerStarvationReportedOncePerEpisode: a chronically wedged
// lane journals one event per stall, not one per tick, and becomes eligible to
// report again only after firing and stalling anew.
func TestScheduledTriggerStarvationReportedOncePerEpisode(t *testing.T) {
	now := time.Date(2026, time.July, 29, 4, 0, 0, 0, time.UTC)
	scheduler, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:         "pr-remediation",
		Gaggle:           "goobers",
		Schedules:        []Schedule{mustParseSchedule(t, "* * * * *")},
		PlacementRefusal: "no runner satisfies the pinned inventory",
	}})
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "pr-remediation"}
	setLastEval(scheduler, identity, now.Add(-time.Hour))

	scheduler.Tick(context.Background(), now)
	scheduler.Tick(context.Background(), now.Add(time.Minute))
	scheduler.Tick(context.Background(), now.Add(2*time.Minute))
	if starved := starvedEvents(t, dir); len(starved) != 1 {
		t.Fatalf("workflow.starved events = %d across three stalled ticks, want one: %+v", len(starved), starved)
	}

	recovered := now.Add(3 * time.Minute)
	setLastEval(scheduler, identity, recovered)
	scheduler.Tick(context.Background(), recovered)
	if starved := starvedEvents(t, dir); len(starved) != 1 {
		t.Fatalf("recovery must not journal another starvation: %+v", starved)
	}

	setLastEval(scheduler, identity, recovered)
	scheduler.Tick(context.Background(), recovered.Add(time.Hour))
	if starved := starvedEvents(t, dir); len(starved) != 2 {
		t.Fatalf("a second stall episode must be reported: %+v", starved)
	}
}

// TestManualOnlyWorkflowIsNeverStarved: a workflow with no schedule has no
// interval to be overdue against, so silence is its normal state.
func TestManualOnlyWorkflowIsNeverStarved(t *testing.T) {
	now := time.Date(2026, time.July, 29, 4, 0, 0, 0, time.UTC)
	scheduler, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:         "manual-only",
		Gaggle:           "goobers",
		PlacementRefusal: "no runner satisfies the pinned inventory",
	}})
	setLastEval(scheduler, WorkflowIdentity{Gaggle: "goobers", Workflow: "manual-only"}, now.Add(-24*time.Hour))

	scheduler.Tick(context.Background(), now)

	if starved := starvedEvents(t, dir); len(starved) != 0 {
		t.Fatalf("manual-only workflow must never be reported starved: %+v", starved)
	}
}

func TestScheduleIntervalTakesShortestSchedule(t *testing.T) {
	from := time.Date(2026, time.July, 29, 4, 0, 0, 0, time.UTC)
	interval, ok := scheduleInterval([]Schedule{
		mustParseSchedule(t, "@hourly"),
		mustParseSchedule(t, "*/10 * * * *"),
	}, from)
	if !ok {
		t.Fatal("scheduleInterval reported no interval for two valid schedules")
	}
	if interval != 10*time.Minute {
		t.Errorf("interval = %s, want the shortest schedule's 10m", interval)
	}
	if _, ok := scheduleInterval(nil, from); ok {
		t.Error("scheduleInterval must report false with no schedules")
	}
}
