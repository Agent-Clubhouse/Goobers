package localscheduler

// Tests for #5564: a scheduled lane whose demand poll sizes every due slot to
// zero stops journaling trigger.fired while LastEval keeps advancing, so
// neither #1868's stall check nor #5277's capacity check could see it.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func newQuietLaneScheduler(t *testing.T, counter BacklogCounter, start time.Time) (*Scheduler, string, *fakeStarter) {
	t.Helper()
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:              "pr-remediation",
		Gaggle:                "goobers",
		Readiness:             apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules:             []Schedule{mustParseSchedule(t, "*/10 * * * *")},
		ScheduleDemandCounter: counter,
		Starter:               starter,
	}})
	setLastEval(sched, WorkflowIdentity{Gaggle: "goobers", Workflow: "pr-remediation"}, start)
	return sched, dir, starter
}

// tickSlots ticks one due */10 slot at a time, from the first slot after
// start through slot n.
func tickSlots(sched *Scheduler, start time.Time, from, to int) {
	for slot := from; slot <= to; slot++ {
		sched.Tick(context.Background(), start.Add(time.Duration(slot)*10*time.Minute))
		sched.Wait()
	}
}

func TestQuietScheduledTriggerJournalsStarvationAfterLastFire(t *testing.T) {
	start := time.Date(2026, time.September, 22, 8, 20, 0, 0, time.UTC)
	counter := &fakeBacklogCounter{count: 1}
	sched, dir, starter := newQuietLaneScheduler(t, counter, start)

	tickSlots(sched, start, 1, 1)
	if starter.count() != 1 {
		t.Fatalf("runs = %d, want the 08:30 fire", starter.count())
	}

	counter.setCount(0)
	tickSlots(sched, start, 2, 5)
	if got := starvedEvents(t, dir); len(got) != 0 {
		t.Fatalf("starved events 40m after the last fire = %+v, want none below 5x", got)
	}

	tickSlots(sched, start, 6, 9)
	starved := starvedEvents(t, dir)
	if len(starved) != 1 {
		t.Fatalf("workflow.starved events = %+v, want exactly one per quiet episode", starved)
	}
	if starved[0].Workflow != "pr-remediation" || starved[0].Gaggle != "goobers" {
		t.Errorf("event must carry the workflow identity: %+v", starved[0])
	}
	reason := starved[0].Reason
	for _, want := range []string{"has not fired for 50m0s", "10m0s schedule interval", "no eligible work on 5 consecutive due slot(s)"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not contain %q", reason, want)
		}
	}
	if starter.count() != 1 {
		t.Fatalf("runs = %d, zero-sized slots must not dispatch", starter.count())
	}
}

func TestQuietScheduledTriggerRearmsAfterFire(t *testing.T) {
	start := time.Date(2026, time.September, 22, 8, 20, 0, 0, time.UTC)
	counter := &fakeBacklogCounter{}
	sched, dir, starter := newQuietLaneScheduler(t, counter, start)

	// No fire seen by this process: the episode opens at the first zero slot.
	tickSlots(sched, start, 1, 5)
	if got := starvedEvents(t, dir); len(got) != 0 {
		t.Fatalf("starved events 40m into the episode = %+v, want none", got)
	}
	tickSlots(sched, start, 6, 6)
	if got := starvedEvents(t, dir); len(got) != 1 {
		t.Fatalf("starved events = %+v, want one at 5x from the first zero slot", got)
	}

	counter.setCount(1)
	tickSlots(sched, start, 7, 7)
	if starter.count() != 1 {
		t.Fatalf("runs = %d, want the recovered fire", starter.count())
	}
	counter.setCount(0)
	tickSlots(sched, start, 8, 11)
	if got := starvedEvents(t, dir); len(got) != 1 {
		t.Fatalf("starved events below 5x after recovery = %+v, want still one", got)
	}
	tickSlots(sched, start, 12, 12)
	if got := starvedEvents(t, dir); len(got) != 2 {
		t.Fatalf("a second quiet episode must be reported: %+v", got)
	}
}

// A failed poll journals its own error and has its own streak alarm (#5605);
// it is not evidence of zero demand. A lane that polled zero once and then
// kept failing must get only the #5605 alarm, not a "no eligible work" one
// that counts the failures as quiet time.
func TestFailedDemandPollIsNotReportedQuiet(t *testing.T) {
	start := time.Date(2026, time.September, 22, 8, 20, 0, 0, time.UTC)
	counter := &fakeBacklogCounter{}
	sched, dir, _ := newQuietLaneScheduler(t, counter, start)

	tickSlots(sched, start, 1, 1)
	counter.mu.Lock()
	counter.err = errors.New("GET /pulls failed: status 404")
	counter.mu.Unlock()
	tickSlots(sched, start, 2, 10)
	counter.mu.Lock()
	counter.err = nil
	counter.mu.Unlock()
	tickSlots(sched, start, 11, 14)

	starved := starvedEvents(t, dir)
	if len(starved) != 1 || !strings.Contains(starved[0].Reason, "schedule_demand_count_failed") {
		t.Fatalf("starved events = %+v, want only the #5605 failure-streak alarm", starved)
	}
}
