package localscheduler

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestTickFansOutScheduledDemandToAvailableCapacity(t *testing.T) {
	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 5}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:              "pr-remediation",
		Readiness:             apiv1.ReadinessConditions{MaxConcurrentRuns: 3},
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		ScheduleDemandCounter: counter,
		Starter:               starter,
	}})

	sched.mu.Lock()
	lastEval := sched.triggers[WorkflowIdentity{Workflow: "pr-remediation"}].LastEval
	sched.mu.Unlock()
	sched.Tick(context.Background(), lastEval.Add(time.Hour))
	waitForCount(t, starter.count, 3)
	close(block)
	sched.Wait()

	if counter.polls() != 1 {
		t.Fatalf("demand polls = %d, want 1", counter.polls())
	}
	if starter.count() != 3 {
		t.Fatalf("scheduled runs = %d, want available capacity 3", starter.count())
	}
}

func TestTickSuppressesScheduledRunWithoutEligibleDemand(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:              "pr-remediation",
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		ScheduleDemandCounter: counter,
		Starter:               starter,
	}})

	sched.mu.Lock()
	lastEval := sched.triggers[WorkflowIdentity{Workflow: "pr-remediation"}].LastEval
	sched.mu.Unlock()
	sched.Tick(context.Background(), lastEval.Add(time.Hour))
	sched.Wait()

	if counter.polls() != 1 {
		t.Fatalf("demand polls = %d, want 1", counter.polls())
	}
	if starter.count() != 0 {
		t.Fatalf("scheduled runs = %d, want no ineligible work dispatched", starter.count())
	}
}
