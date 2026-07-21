package localscheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestTickRoundRobinSharesInstanceCapacity(t *testing.T) {
	block := make(chan struct{})
	var release sync.Once
	releaseRuns := func() { release.Do(func() { close(block) }) }
	t.Cleanup(releaseRuns)

	saturating := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	single := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:       "a-saturating",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 5},
			BacklogCounter: &fakeBacklogCounter{count: 5},
			Starter:        saturating,
		},
		{
			Workflow:       "b-single",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			BacklogCounter: &fakeBacklogCounter{count: 1},
			Starter:        single,
		},
	})
	sched.conditions.SetInstanceLimits(3, nil, nil)

	sched.Tick(context.Background(), time.Now())
	releaseRuns()
	sched.Wait()

	if saturating.count() != 2 || single.count() != 1 {
		t.Fatalf("starts = saturating:%d single:%d, want 2 and 1", saturating.count(), single.count())
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			order = append(order, event.Workflow)
		}
	}
	want := []string{"a-saturating", "b-single", "a-saturating"}
	if len(order) != len(want) {
		t.Fatalf("run.started order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("run.started order = %v, want %v", order, want)
		}
	}
	if len(sched.consecutivePoolSkips) != 0 {
		t.Fatalf("successful workflows should not age from unused demand: %v", sched.consecutivePoolSkips)
	}
}

func TestTickRunsAfterTickAfterFairDispatch(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	afterTickCalls := 0
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "backlogged",
		Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
		BacklogCounter: &fakeBacklogCounter{count: 2},
		Starter:        starter,
	}}, WithAfterTick(func(context.Context) {
		afterTickCalls++
	}))

	sched.Tick(context.Background(), time.Now())
	sched.Wait()

	if got := starter.count(); got != 2 {
		t.Fatalf("starts = %d, want 2 fair-dispatch passes", got)
	}
	if afterTickCalls != 1 {
		t.Fatalf("after-tick calls = %d, want 1", afterTickCalls)
	}
}

func TestTickPrioritizesAgedWorkflowWhenCapacityReturns(t *testing.T) {
	aBlock := make(chan struct{})
	bBlock := make(chan struct{})
	var releaseA, releaseB sync.Once
	releaseARuns := func() { releaseA.Do(func() { close(aBlock) }) }
	releaseBRuns := func() { releaseB.Do(func() { close(bBlock) }) }
	t.Cleanup(releaseARuns)
	t.Cleanup(releaseBRuns)

	a := &fakeStarter{block: aBlock, result: StartResult{Phase: journal.PhaseCompleted}}
	b := &fakeStarter{block: bBlock, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:       "a-workflow",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
			BacklogCounter: &fakeBacklogCounter{count: 2},
			Starter:        a,
		},
		{
			Workflow:       "b-workflow",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
			BacklogCounter: &fakeBacklogCounter{count: 2},
			Starter:        b,
		},
	})
	sched.conditions.SetInstanceLimits(1, nil, nil)

	now := time.Now()
	sched.Tick(context.Background(), now)
	if got := sched.consecutivePoolSkips[WorkflowIdentity{Workflow: "b-workflow"}]; got != 1 {
		t.Fatalf("b-workflow pool skips = %d, want 1", got)
	}
	releaseARuns()
	sched.Wait()

	sched.Tick(context.Background(), now.Add(backlogPollInterval+time.Second))
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("starts after capacity returned = a:%d b:%d, want 1 and 1", a.count(), b.count())
	}
	if got := sched.consecutivePoolSkips[WorkflowIdentity{Workflow: "a-workflow"}]; got != 1 {
		t.Fatalf("a-workflow pool skips = %d, want 1 after b-workflow took the returned slot", got)
	}
	if got := sched.consecutivePoolSkips[WorkflowIdentity{Workflow: "b-workflow"}]; got != 0 {
		t.Fatalf("b-workflow pool skips = %d, want reset after dispatch", got)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			order = append(order, event.Workflow)
		}
	}
	if len(order) != 2 || order[0] != "a-workflow" || order[1] != "b-workflow" {
		t.Fatalf("run.started order = %v, want [a-workflow b-workflow]", order)
	}
	releaseBRuns()
	sched.Wait()
}

func TestTickDoesNotAgeNonPoolRefusals(t *testing.T) {
	t.Run("workflow cap", func(t *testing.T) {
		block := make(chan struct{})
		var release sync.Once
		releaseRun := func() { release.Do(func() { close(block) }) }
		t.Cleanup(releaseRun)

		starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
		sched, _ := newTestScheduler(t, []WorkflowEntry{{
			Workflow:       "capped",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			BacklogCounter: &fakeBacklogCounter{count: 1},
			Starter:        starter,
		}})
		sched.conditions.SetInstanceLimits(2, nil, nil)

		if _, err := sched.Trigger(context.Background(), "capped", time.Now()); err != nil {
			t.Fatal(err)
		}
		sched.Tick(context.Background(), time.Now())
		if got := sched.consecutivePoolSkips[WorkflowIdentity{Workflow: "capped"}]; got != 0 {
			t.Fatalf("pool skips = %d, want 0 for a workflow-cap refusal", got)
		}
		releaseRun()
		sched.Wait()
	})

	t.Run("budget while pool full", func(t *testing.T) {
		holderBlock := make(chan struct{})
		var release sync.Once
		releaseHolder := func() { release.Do(func() { close(holderBlock) }) }
		t.Cleanup(releaseHolder)

		budgeted := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
		holder := &fakeStarter{block: holderBlock, result: StartResult{Phase: journal.PhaseCompleted}}
		sched, dir := newTestScheduler(t, []WorkflowEntry{
			{
				Workflow:       "budgeted",
				Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 1},
				BacklogCounter: &fakeBacklogCounter{count: 1},
				Starter:        budgeted,
			},
			{
				Workflow:  "holder",
				Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
				Starter:   holder,
			},
		})
		sched.conditions.SetInstanceLimits(1, nil, nil)

		now := time.Now()
		if _, err := sched.Trigger(context.Background(), "budgeted", now); err != nil {
			t.Fatal(err)
		}
		sched.Wait()
		if _, err := sched.Trigger(context.Background(), "holder", now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		waitForCount(t, holder.count, 1)

		sched.Tick(context.Background(), now.Add(2*time.Second))
		if got := sched.consecutivePoolSkips[WorkflowIdentity{Workflow: "budgeted"}]; got != 0 {
			t.Fatalf("pool skips = %d, want 0 for a budget refusal", got)
		}
		events, err := journal.ReadInstanceLog(dir)
		if err != nil {
			t.Fatal(err)
		}
		var sawBudgetSkip bool
		for _, event := range events {
			if event.Type == journal.EventTickSkipped && event.Workflow == "budgeted" && event.Reason == ReasonBudget {
				sawBudgetSkip = true
			}
		}
		if !sawBudgetSkip {
			t.Fatalf("expected budget refusal while the pool was full: %+v", events)
		}
		releaseHolder()
		sched.Wait()
	})

	t.Run("no ready demand", func(t *testing.T) {
		sched, _ := newTestScheduler(t, []WorkflowEntry{{
			Workflow:       "empty",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			BacklogCounter: &fakeBacklogCounter{},
			Starter:        &fakeStarter{},
		}})
		sched.Tick(context.Background(), time.Now())
		if len(sched.consecutivePoolSkips) != 0 {
			t.Fatalf("non-ready workflow aged: %v", sched.consecutivePoolSkips)
		}
	})
}

func TestStarvationEventEmittedOnceAtThresholdCrossing(t *testing.T) {
	holderBlock := make(chan struct{})
	var release sync.Once
	releaseHolder := func() { release.Do(func() { close(holderBlock) }) }
	t.Cleanup(releaseHolder)

	holder := &fakeStarter{block: holderBlock, result: StartResult{Phase: journal.PhaseCompleted}}
	starved := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:  "holder",
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:   holder,
		},
		{
			Workflow:       "starved",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			BacklogCounter: &fakeBacklogCounter{count: 1},
			Starter:        starved,
		},
	})
	sched.conditions.SetInstanceLimits(1, nil, nil)

	now := time.Now()
	if _, err := sched.Trigger(context.Background(), "holder", now); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, holder.count, 1)
	for i := 0; i < starvationSkipThreshold+1; i++ {
		sched.Tick(context.Background(), now.Add(time.Duration(i)*(backlogPollInterval+time.Second)))
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var starvationEvents []journal.Event
	for _, event := range events {
		if event.Type == journal.EventWorkflowStarved {
			starvationEvents = append(starvationEvents, event)
		}
	}
	if len(starvationEvents) != 1 {
		t.Fatalf("workflow.starved events = %d, want 1: %+v", len(starvationEvents), starvationEvents)
	}
	event := starvationEvents[0]
	if event.Workflow != "starved" || event.SkipCount != starvationSkipThreshold {
		t.Fatalf("workflow.starved event = %+v, want workflow=starved skipCount=%d", event, starvationSkipThreshold)
	}

	releaseHolder()
	sched.Wait()
	sched.Tick(context.Background(), now.Add(5*(backlogPollInterval+time.Second)))
	sched.Wait()
	if got := sched.consecutivePoolSkips[WorkflowIdentity{Workflow: "starved"}]; got != 0 {
		t.Fatalf("pool skips = %d, want reset after successful dispatch", got)
	}
}
