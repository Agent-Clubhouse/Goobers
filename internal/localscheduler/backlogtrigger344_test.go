package localscheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// fakeBacklogCounter is a scripted BacklogCounter double: EligibleCount
// returns count (or err, if set) every time it's called, and records how
// many times it was invoked.
type fakeBacklogCounter struct {
	mu     sync.Mutex
	count  int
	err    error
	polled int
}

func (f *fakeBacklogCounter) EligibleCount(ctx context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polled++
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
