package localscheduler

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// fakeStarter records every Start call and returns a canned result. It blocks
// on a channel if one is set, so tests can control exactly when a run
// "finishes" and its condition slot is released.
type fakeStarter struct {
	mu     sync.Mutex
	starts []StartRequest
	block  chan struct{} // if non-nil, Start waits on it before returning
	result StartResult
	err    error
}

func (f *fakeStarter) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	f.mu.Lock()
	f.starts = append(f.starts, req)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return f.result, f.err
}

func (f *fakeStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func newTestScheduler(t *testing.T, entries []WorkflowEntry) (*Scheduler, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return New(entries, log), dir
}

func TestTickDispatchesDueWorkflow(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedule:  fakeSchedule{d: time.Hour},
		Starter:   starter,
	}})

	now := time.Now()
	sched.Tick(context.Background(), now.Add(2*time.Hour))

	waitForCount(t, func() int { return starter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawFired, sawStarted bool
	for _, ev := range events {
		if ev.Type == journal.EventTriggerFired && ev.Workflow == "implement" {
			sawFired = true
		}
		if ev.Type == journal.EventRunStarted && ev.Workflow == "implement" {
			sawStarted = true
		}
	}
	if !sawFired || !sawStarted {
		t.Fatalf("expected trigger.fired + run.started journaled: %+v", events)
	}
}

func TestTickSkipsWhenConditionsExhausted(t *testing.T) {
	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedule:  fakeSchedule{d: time.Hour},
		Starter:   starter,
	}})

	base := time.Now()
	// First tick admits and starts a run that blocks (holds its slot).
	sched.Tick(context.Background(), base.Add(time.Hour))
	waitForCount(t, func() int { return starter.count() }, 1)

	// Second due tick, one MaxConcurrentRuns=1 slot already held: must skip.
	sched.Tick(context.Background(), base.Add(2*time.Hour))
	close(block) // release the first run so the test can exit cleanly

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var skipped bool
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && ev.Reason == ReasonMaxParallel {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("expected a tick.skipped(max-parallel) event: %+v", events)
	}
	if starter.count() != 1 {
		t.Fatalf("starter should have been called exactly once, got %d", starter.count())
	}
}

func TestManualTriggerBypassesCronButHonorsConditions(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedule:  nil, // manual-only: Tick alone would never fire this
		Starter:   starter,
	}})

	// A cron Tick does nothing for a manual-only workflow.
	sched.Tick(context.Background(), time.Now())
	if starter.count() != 0 {
		t.Fatalf("manual-only workflow should not fire from Tick: %d starts", starter.count())
	}

	if err := sched.Trigger(context.Background(), "curate", time.Now()); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	waitForCount(t, func() int { return starter.count() }, 1)
}

func TestTriggerUnknownWorkflowErrors(t *testing.T) {
	sched, _ := newTestScheduler(t, nil)
	if err := sched.Trigger(context.Background(), "nope", time.Now()); err == nil {
		t.Fatal("expected an error for an unknown workflow")
	}
}

// TestTickSkipsOnBudgetExhaustion is the budget half of the run-conditions
// acceptance criterion (the max-parallel half is TestTickSkipsWhenConditions
// Exhausted): once MaxRunsPerHour is spent, further due ticks skip and journal
// ReasonBudget, never fail.
func TestTickSkipsOnBudgetExhaustion(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 1},
		Schedule:  fakeSchedule{d: time.Hour},
		Starter:   starter,
	}})

	base := time.Now()
	sched.Tick(context.Background(), base.Add(time.Hour)) // uses the hourly budget
	waitForCount(t, func() int { return starter.count() }, 1)

	sched.Tick(context.Background(), base.Add(2*time.Hour)) // due again, budget spent

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawBudgetSkip bool
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && ev.Reason == ReasonBudget {
			sawBudgetSkip = true
		}
	}
	if !sawBudgetSkip {
		t.Fatalf("expected a tick.skipped(budget) event: %+v", events)
	}
	if starter.count() != 1 {
		t.Fatalf("starter should have been called exactly once (budget exhausted), got %d", starter.count())
	}
}

// TestMissedTickCatchUpJournaled is the missed-tick acceptance criterion at
// the Scheduler level: daemon downtime spanning several scheduled fires
// produces exactly one catch-up run, and the journaled trigger.fired event
// records it as a catch-up (not silently indistinguishable from an on-time fire).
func TestMissedTickCatchUpJournaled(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "nominate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedule:  fakeSchedule{d: time.Hour},
		Starter:   starter,
	}})

	base := time.Now()
	// Simulate the daemon having been down for 5 scheduled fires.
	sched.Tick(context.Background(), base.Add(5*time.Hour))
	waitForCount(t, func() int { return starter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var catchUpReason string
	for _, ev := range events {
		if ev.Type == journal.EventTriggerFired && ev.Workflow == "nominate" {
			catchUpReason = ev.Reason
		}
	}
	if catchUpReason == "" || catchUpReason == "scheduled" {
		t.Fatalf("expected the fire to be journaled as a catch-up, got reason %q", catchUpReason)
	}

	// The very next tick must not replay a backlog of the missed fires.
	sched.Tick(context.Background(), base.Add(5*time.Hour+time.Minute))
	if starter.count() != 1 {
		t.Fatalf("missed-tick collapse must not leave a backlog to replay: %d starts", starter.count())
	}
}

// TestRunDoesNotBusyPoll is the "no busy-polling: daemon idles between ticks"
// acceptance criterion: drive Run with a fake clock/timer and assert (a) it
// only re-evaluates when the injected timer channel fires — never more often
// — and (b) every requested wait duration is strictly positive, proving the
// loop blocks on a timer rather than spinning.
func TestRunDoesNotBusyPoll(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	base := time.Now()
	var mu sync.Mutex
	cur := base
	fc := newFakeClock(cur)

	sched := New([]WorkflowEntry{{
		Workflow:  "wf",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100},
		Schedule:  fakeSchedule{d: time.Hour},
		Starter:   starter,
	}}, log, WithClock(fc.Now, fc.After))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Advance through exactly 3 simulated ticks, each firing the workflow.
	for i := 0; i < 3; i++ {
		fc.awaitAfterCall(t)
		mu.Lock()
		cur = cur.Add(time.Hour)
		mu.Unlock()
		fc.advance(cur)
	}
	// Wait on the actual observable the assertion below checks — dispatch()
	// hands the Starter.Start call to its own goroutine (Run must never
	// block on a run), so the loop registering its next After call (what a
	// 4th awaitAfterCall would prove) does NOT happen-before that goroutine
	// incrementing starter's count. Waiting on the count itself closes that
	// race deterministically instead of relying on tick timing as a proxy.
	waitForCount(t, starter.count, 3)
	cancel()
	<-done

	if got := starter.count(); got < 3 {
		t.Fatalf("expected at least 3 dispatches from 3 controlled ticks, got %d", got)
	}
	for i, d := range fc.durations() {
		if d <= 0 {
			t.Fatalf("After call %d requested a non-positive duration %v — would busy-loop", i, d)
		}
	}
	// The number of After calls should track the number of controlled fires
	// (initial tick + one per advance), not run away on its own.
	if calls := len(fc.durations()); calls > 6 {
		t.Fatalf("too many After calls (%d) for 4 controlled advances — looks like busy-polling", calls)
	}
}

func waitForCount(t *testing.T, count func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for count >= %d, got %d", want, count())
}

// fakeClock is a controllable Clock for no-busy-poll tests: Now() reads an
// atomically-updated instant, After() records the requested duration and
// returns a channel the test fires manually via advance().
type fakeClock struct {
	now atomic.Pointer[time.Time]

	mu       sync.Mutex
	ch       chan time.Time
	waiting  chan struct{}
	requests []time.Duration
}

func newFakeClock(start time.Time) *fakeClock {
	f := &fakeClock{ch: make(chan time.Time), waiting: make(chan struct{}, 8)}
	f.now.Store(&start)
	return f
}

func (f *fakeClock) Now() time.Time { return *f.now.Load() }

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	f.requests = append(f.requests, d)
	f.mu.Unlock()
	select {
	case f.waiting <- struct{}{}:
	default:
	}
	return f.ch
}

// awaitAfterCall blocks until Run has called After at least once since the
// last advance (i.e. it's idling on the timer, ready for the next controlled fire).
func (f *fakeClock) awaitAfterCall(t *testing.T) {
	t.Helper()
	select {
	case <-f.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the scheduler loop to call After (idle-between-ticks)")
	}
}

// advance sets Now to t and fires the pending After channel once.
func (f *fakeClock) advance(t time.Time) {
	f.now.Store(&t)
	f.ch <- t
}

func (f *fakeClock) durations() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.requests...)
}
