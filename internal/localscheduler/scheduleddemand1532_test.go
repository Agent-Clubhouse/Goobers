package localscheduler

import (
	"context"
	"errors"
	"path/filepath"
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

func TestTickClearsPersistedDemandAfterFullDispatch(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 1}
	sched, schedulerDir := newTestScheduler(t, []WorkflowEntry{{
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

	outstanding, err := readScheduleDemandState(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if outstanding[WorkflowIdentity{Workflow: "pr-remediation"}] {
		t.Fatal("fully dispatched schedule demand remained outstanding")
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

func TestReconcileRepollsScheduledDemandBeforeNextFire(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	runsDir := t.TempDir()
	active, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           "active-run",
		Gaggle:          "goobers",
		Workflow:        "pr-remediation",
		WorkflowVersion: 1,
		Trigger:         journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := active.Close(); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{
		Type:     journal.EventTriggerFired,
		Gaggle:   "goobers",
		Workflow: "pr-remediation",
		Reason:   triggerReasonScheduled,
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeScheduleDemandState(schedulerDir, nil, map[WorkflowIdentity]bool{
		{Gaggle: "goobers", Workflow: "pr-remediation"}: true,
	}); err != nil {
		t.Fatal(err)
	}

	log, _, err = journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 2}
	sched := New([]WorkflowEntry{{
		Gaggle:                "goobers",
		Workflow:              "pr-remediation",
		Readiness:             apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		ScheduleDemandCounter: counter,
		Starter:               starter,
	}}, log)
	restartedAt := time.Now().Add(30 * time.Minute)
	if err := sched.Reconcile(runsDir, restartedAt); err != nil {
		t.Fatal(err)
	}

	sched.Tick(context.Background(), restartedAt)
	if starter.count() != 0 {
		t.Fatalf("scheduled runs = %d, want reconciled run to hold capacity", starter.count())
	}

	sched.ReleaseReconciled("active-run", "pr-remediation")
	sched.Tick(context.Background(), restartedAt)
	waitForCount(t, starter.count, 1)
	close(block)
	sched.Wait()

	if counter.polls() != 1 {
		t.Fatalf("demand polls = %d, want recovered demand retained after the original poll", counter.polls())
	}
	if starter.count() != 1 {
		t.Fatalf("scheduled runs = %d, want recovered demand to refill released capacity before the next firing", starter.count())
	}
}

func TestReconcileDoesNotReplayConsumedScheduledDemand(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	runsDir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := log.Append(journal.Event{
		Type:     journal.EventTriggerFired,
		Gaggle:   "goobers",
		Workflow: "pr-remediation",
		Reason:   triggerReasonScheduled,
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{
		Type:     journal.EventRunStarted,
		Gaggle:   "goobers",
		Workflow: "pr-remediation",
		RunID:    "completed-run",
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	log, _, err = journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 2}
	sched := New([]WorkflowEntry{{
		Gaggle:                "goobers",
		Workflow:              "pr-remediation",
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		ScheduleDemandCounter: counter,
		Starter:               starter,
	}}, log)
	restartedAt := time.Now().Add(30 * time.Minute)
	if err := sched.Reconcile(runsDir, restartedAt); err != nil {
		t.Fatal(err)
	}

	sched.Tick(context.Background(), restartedAt)
	sched.Wait()
	if counter.polls() != 0 {
		t.Fatalf("demand polls = %d, want consumed historical firing ignored", counter.polls())
	}
	if starter.count() != 0 {
		t.Fatalf("scheduled runs = %d, want no replay before the next firing", starter.count())
	}
}

func TestReloadClearsRemovedWorkflowScheduleDemand(t *testing.T) {
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "pr-remediation"}
	sched, schedulerDir := newTestScheduler(t, []WorkflowEntry{{
		Gaggle:                identity.Gaggle,
		Workflow:              identity.Workflow,
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		ScheduleDemandCounter: &fakeBacklogCounter{count: 1},
		Starter:               &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}},
	}})
	sched.mu.Lock()
	sched.pendingScheduleDemand[identity] = scheduledDemand{remaining: 1}
	sched.mu.Unlock()
	if err := writeScheduleDemandState(schedulerDir, nil, map[WorkflowIdentity]bool{identity: true}); err != nil {
		t.Fatal(err)
	}

	if err := sched.Reload(nil, nil, time.Now(), "old", "new"); err != nil {
		t.Fatal(err)
	}
	outstanding, err := readScheduleDemandState(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if outstanding[identity] {
		t.Fatal("removed workflow retained durable schedule demand")
	}
}
