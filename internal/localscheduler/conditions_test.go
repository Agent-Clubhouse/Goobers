package localscheduler

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestAdmitDefaultMaxConcurrentIsOne(t *testing.T) {
	c := NewConditions()
	now := time.Now()
	ok, reason := c.Admit("wf", apiv1.ReadinessConditions{}, now)
	if !ok {
		t.Fatalf("first admit should succeed: %s", reason)
	}
	ok, reason = c.Admit("wf", apiv1.ReadinessConditions{}, now)
	if ok || reason != ReasonMaxParallel {
		t.Fatalf("second admit with default max=1 should be refused: ok=%v reason=%s", ok, reason)
	}
}

func TestAdmitReleaseFreesSlot(t *testing.T) {
	c := NewConditions()
	now := time.Now()
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 1}
	ok, _ := c.Admit("wf", r, now)
	if !ok {
		t.Fatal("expected admit")
	}
	if ok, _ := c.Admit("wf", r, now); ok {
		t.Fatal("expected refusal while slot held")
	}
	c.Release("wf")
	if ok, reason := c.Admit("wf", r, now); !ok {
		t.Fatalf("expected admit after release: %s", reason)
	}
}

// TestMaxParallelHoldsUnderSimultaneousTicks is the concurrency acceptance
// criterion: N goroutines race Admit for the same workflow with
// MaxConcurrentRuns=K; exactly K must succeed, never more (the check-and-
// reserve must be atomic, not check-then-increment across a race window).
func TestMaxParallelHoldsUnderSimultaneousTicks(t *testing.T) {
	const workers = 200
	const limit = 5
	c := NewConditions()
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: limit}
	now := time.Now()

	var admitted int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, _ := c.Admit("wf", r, now); ok {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if admitted != limit {
		t.Fatalf("admitted = %d, want exactly %d", admitted, limit)
	}
	if got := c.Active("wf"); got != limit {
		t.Fatalf("active count = %d, want %d", got, limit)
	}
}

func TestAdmitBudgetWindow(t *testing.T) {
	c := NewConditions()
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 2}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	ok, _ := c.Admit("wf", r, base)
	if !ok {
		t.Fatal("1st run in budget should admit")
	}
	c.Release("wf")
	ok, _ = c.Admit("wf", r, base.Add(time.Minute))
	if !ok {
		t.Fatal("2nd run in budget should admit")
	}
	c.Release("wf")
	ok, reason := c.Admit("wf", r, base.Add(2*time.Minute))
	if ok || reason != ReasonBudget {
		t.Fatalf("3rd run should exhaust the hourly budget: ok=%v reason=%s", ok, reason)
	}

	// Outside the rolling window, the budget resets.
	ok, reason = c.Admit("wf", r, base.Add(90*time.Minute))
	if !ok {
		t.Fatalf("run outside the rolling window should admit: %s", reason)
	}
}

// TestAdmitWithoutBudgetDoesNotAccumulateStarts is issue #138's unbounded-
// growth fix: a workflow with no MaxRunsPerHour configured must never grow
// Conditions' starts map, since nothing ever prunes it for that workflow
// (Admit's own prune only runs inside the MaxRunsPerHour>0 branch) — before
// this fix, a schedule like `@every 1m` with no budget set would accumulate
// ~1,440 unpruned entries per day for the life of the daemon.
func TestAdmitWithoutBudgetDoesNotAccumulateStarts(t *testing.T) {
	c := NewConditions()
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 1000} // no MaxRunsPerHour
	now := time.Now()

	for i := 0; i < 50; i++ {
		ok, reason := c.Admit("wf", r, now)
		if !ok {
			t.Fatalf("admit %d should succeed: %s", i, reason)
		}
		c.Release("wf")
	}

	c.mu.Lock()
	got := len(c.starts["wf"])
	c.mu.Unlock()
	if got != 0 {
		t.Fatalf("starts[wf] = %d entries, want 0 — a workflow with no MaxRunsPerHour must never accumulate starts", got)
	}
}

func TestReconcileSeedsActiveCounts(t *testing.T) {
	c := NewConditions()
	c.Reconcile(map[string]int{"wf": 3})
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 3}
	if ok, reason := c.Admit("wf", r, time.Now()); ok {
		t.Fatalf("reconciled active count should already be at the limit: ok=%v reason=%s", ok, reason)
	}
}
