package localscheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/providersnapshot"
)

// fakeBacklogCounter is a scripted BacklogCounter double: EligibleCount
// returns count (or err, if set) every time it's called, and records how
// many times it was invoked.
type fakeBacklogCounter struct {
	mu          sync.Mutex
	count       int
	err         error
	polled      int
	snapshotIDs []string
}

func (f *fakeBacklogCounter) EligibleCount(ctx context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polled++
	f.snapshotIDs = append(f.snapshotIDs, providersnapshot.ID(ctx))
	if f.err != nil {
		return 0, f.err
	}
	return f.count, nil
}

func (f *fakeBacklogCounter) polls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polled
}

func (f *fakeBacklogCounter) snapshots() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.snapshotIDs...)
}

// TestTickFansOutBacklogTriggeredWorkflow is #344's core acceptance
// scenario: with maxConcurrentRuns:3 and 5 ready backlog items, a single
// Tick evaluation dispatches 3 concurrent runs, not 1 — the exact fix for
// "one trigger firing = at most one new run, always" the issue reports.
func TestTickFansOutBacklogTriggeredWorkflow(t *testing.T) {
	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 5}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "implement",
		Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 3},
		BacklogCounter: counter,
		Starter:        starter,
	}})

	sched.Tick(context.Background(), time.Now())
	waitForCount(t, func() int { return starter.count() }, 3)
	close(block)

	if got := starter.count(); got != 3 {
		t.Fatalf("dispatched %d runs, want exactly 3 (bounded by maxConcurrentRuns, not the 5 ready items)", got)
	}
	snapshotIDs := starter.snapshots()
	if len(snapshotIDs) != 3 || snapshotIDs[0] == "" || snapshotIDs[0] != snapshotIDs[1] || snapshotIDs[0] != snapshotIDs[2] {
		t.Fatalf("run snapshot IDs = %v, want one shared non-empty tick snapshot", snapshotIDs)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	fired, started, skipped := 0, 0, 0
	for _, ev := range events {
		switch ev.Type {
		case journal.EventTriggerFired:
			fired++
		case journal.EventRunStarted:
			started++
		case journal.EventTickSkipped:
			skipped++
		}
	}
	if started != 3 {
		t.Fatalf("run.started count = %d, want 3", started)
	}
	// The fan-out loop stops at the FIRST refusal (every later attempt in
	// the same evaluation would be refused for the identical reason, so
	// there is no reason to keep trying) — that 4th attempt still gets a
	// trigger.fired (dispatch journals it before checking Admit) followed
	// by a tick.skipped, a recorded decision, not a silent stop; the 5th
	// ready item is never attempted at all.
	if fired != 4 {
		t.Fatalf("trigger.fired count = %d, want 4 (3 admitted + 1 refused attempt before the loop stops)", fired)
	}
	if skipped != 1 {
		t.Fatalf("tick.skipped count = %d, want 1 (the one refused attempt)", skipped)
	}
}

// TestTickBacklogPolledAtMostOncePerInterval confirms backlogPollInterval
// actually throttles the (real, rate-limited) provider call: calling Tick
// repeatedly in quick succession must not re-poll EligibleCount every time.
func TestTickBacklogPolledAtMostOncePerInterval(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 1}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "implement",
		Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 100},
		BacklogCounter: counter,
		Starter:        starter,
	}})

	now := time.Now()
	sched.Tick(context.Background(), now)
	sched.Tick(context.Background(), now.Add(time.Second))
	sched.Tick(context.Background(), now.Add(2*time.Second))
	waitForCount(t, func() int { return starter.count() }, 1)

	if polls := counter.polls(); polls != 1 {
		t.Fatalf("EligibleCount polled %d times across 3 rapid Ticks, want 1 (throttled by backlogPollInterval)", polls)
	}

	// Outside the interval, the next Tick polls again.
	sched.Tick(context.Background(), now.Add(backlogPollInterval+time.Second))
	waitForCount(t, func() int { return counter.polls() }, 2)
}

func TestTickSharesProviderSnapshotAcrossBacklogConsumers(t *testing.T) {
	first := &fakeBacklogCounter{}
	second := &fakeBacklogCounter{}
	sched, _ := newTestScheduler(t, []WorkflowEntry{
		{Workflow: "first", BacklogCounter: first},
		{Workflow: "second", BacklogCounter: second},
	})

	sched.Tick(context.Background(), time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC))

	firstIDs := first.snapshots()
	secondIDs := second.snapshots()
	if len(firstIDs) != 1 || len(secondIDs) != 1 || firstIDs[0] == "" || firstIDs[0] != secondIDs[0] {
		t.Fatalf("snapshot IDs = %v and %v, want one shared non-empty ID", firstIDs, secondIDs)
	}
}

// TestTickBacklogCounterErrorDoesNotCrashOrDispatch confirms a
// BacklogCounter error is journaled, not silently swallowed, and dispatches
// nothing for that evaluation — an intermittent provider failure must not
// look like "zero ready items forever" without a trace, but also must not
// take down the daemon's tick loop.
func TestTickBacklogCounterErrorDoesNotCrashOrDispatch(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{err: errors.New("provider unavailable")}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "implement",
		Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 3},
		BacklogCounter: counter,
		Starter:        starter,
	}})

	sched.Tick(context.Background(), time.Now())
	time.Sleep(50 * time.Millisecond) // let any (unwanted) dispatch goroutine start

	if got := starter.count(); got != 0 {
		t.Fatalf("dispatched %d runs despite a BacklogCounter error, want 0", got)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	sawErr := false
	for _, ev := range events {
		if ev.Type == journal.EventError && ev.Workflow == "implement" && ev.Error != nil && ev.Error.Code == "backlog_count_failed" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatalf("expected a backlog_count_failed error event journaled, got: %+v", events)
	}
}

func TestTickRefillMaintainsDesiredConcurrencyWithoutExternalTrigger(t *testing.T) {
	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 10}
	scheduler, _ := newTestScheduler(t, []WorkflowEntry{{
		Gaggle:              "goobers",
		Workflow:            "implementation",
		Readiness:           apiv1.ReadinessConditions{MaxConcurrentRuns: 4, DesiredConcurrentRuns: 2},
		RefillDemandCounter: counter,
		Starter:             starter,
	}})
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	scheduler.Tick(context.Background(), now)
	waitForCount(t, starter.count, 2)
	if got := counter.polls(); got != 1 {
		t.Fatalf("refill eligibility polls = %d, want one bounded provider read", got)
	}
	close(block)
	scheduler.Wait()

	if got := starter.count(); got != 2 {
		t.Fatalf("refill starts = %d, want 2 runs at desired occupancy", got)
	}
}

func TestTerminalRunWakesDesiredConcurrencyRefill(t *testing.T) {
	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 10}
	scheduler, _ := newTestScheduler(t, []WorkflowEntry{{
		Gaggle:              "goobers",
		Workflow:            "implementation",
		Readiness:           apiv1.ReadinessConditions{MaxConcurrentRuns: 4, DesiredConcurrentRuns: 2},
		RefillDemandCounter: counter,
		Starter:             starter,
	}})
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	scheduler.Tick(context.Background(), now)
	waitForCount(t, starter.count, 2)
	block <- struct{}{}
	select {
	case <-scheduler.wake:
	case <-time.After(time.Second):
		t.Fatal("terminal run did not wake desired-concurrency refill")
	}

	scheduler.Tick(context.Background(), now.Add(time.Second))
	waitForCount(t, starter.count, 3)
	if got := counter.polls(); got != 2 {
		t.Fatalf("refill eligibility polls = %d, want immediate terminal-driven repoll", got)
	}
	close(block)
	scheduler.Wait()
}

func TestTickRefillBudgetRejectionUsesJitteredBackoff(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &fakeBacklogCounter{count: 10}
	scheduler, dir := newTestScheduler(t, []WorkflowEntry{{
		Gaggle:              "goobers",
		Workflow:            "implementation",
		Readiness:           apiv1.ReadinessConditions{MaxConcurrentRuns: 4, MaxRunsPerHour: 1, DesiredConcurrentRuns: 2},
		RefillDemandCounter: counter,
		Starter:             starter,
	}})
	identity := WorkflowIdentity{Gaggle: "goobers", Workflow: "implementation"}
	scheduler.refillBackoff = 30 * time.Second
	scheduler.refillBackoffJitter = 10 * time.Second
	scheduler.refillRandN = func(int64) int64 { return int64(5 * time.Second) }
	started := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	scheduler.Tick(context.Background(), started)
	scheduler.Wait()
	waitForCount(t, starter.count, 1)
	scheduler.mu.Lock()
	retryAt := scheduler.refillBlockedUntil[identity]
	scheduler.mu.Unlock()
	if !retryAt.Equal(started.Add(35 * time.Second)) {
		t.Fatalf("refill retryAt = %s, want %s", retryAt, started.Add(35*time.Second))
	}
	countFired := func() int {
		events, err := journal.ReadInstanceLog(dir)
		if err != nil {
			t.Fatal(err)
		}
		fired := 0
		sawRefillSkip := false
		for _, event := range events {
			if event.Type == journal.EventTriggerFired && event.Workflow == "implementation" {
				fired++
			}
			if event.Type == journal.EventTickSkipped &&
				event.Workflow == "implementation" &&
				event.Reason == "refill blocked: "+ReasonBudget {
				sawRefillSkip = true
			}
		}
		if !sawRefillSkip {
			t.Fatalf("events = %+v, want refill blocked reason", events)
		}
		return fired
	}
	if got := countFired(); got != 2 {
		t.Fatalf("first tick trigger.fired = %d, want 2 (one admitted + one budget refusal)", got)
	}

	scheduler.Tick(context.Background(), started.Add(31*time.Second))
	scheduler.Wait()
	if got := countFired(); got != 2 {
		t.Fatalf("trigger.fired during backoff = %d, want unchanged", got)
	}
	if got := counter.polls(); got != 1 {
		t.Fatalf("eligibility polls during backoff = %d, want unchanged", got)
	}

	scheduler.Tick(context.Background(), started.Add(36*time.Second))
	scheduler.Wait()
	if got := countFired(); got != 3 {
		t.Fatalf("trigger.fired after backoff = %d, want one retry", got)
	}
	if got := counter.polls(); got != 2 {
		t.Fatalf("eligibility polls after backoff = %d, want 2", got)
	}
}
