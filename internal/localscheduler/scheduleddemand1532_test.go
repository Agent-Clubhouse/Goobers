package localscheduler

import (
	"context"
	"errors"
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

func TestRunRefillsScheduledDemandWhenCapacityIsReleased(t *testing.T) {
	start := time.Date(2026, time.July, 25, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 5}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:              "pr-remediation",
		Readiness:             apiv1.ReadinessConditions{MaxConcurrentRuns: 3},
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		ScheduleDemandCounter: counter,
		Starter:               starter,
	}}, WithClock(clock.Now, clock.After))

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- sched.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-runErr; !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context cancellation", err)
		}
		close(block)
		sched.Wait()
	})

	clock.awaitAfterCall(t)
	due := start.Add(time.Hour)
	clock.advance(due)
	waitForCount(t, starter.count, 3)

	block <- struct{}{}
	waitForCount(t, starter.count, 4)

	if !clock.Now().Equal(due) {
		t.Fatalf("clock advanced to %s, want refill before next scheduled firing at %s", clock.Now(), due.Add(time.Hour))
	}
	if counter.polls() != 1 {
		t.Fatalf("demand polls = %d, want original due poll reused", counter.polls())
	}
}
