package localscheduler

import (
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Reason strings for a skipped tick (SCH-004 backpressure) — stable, so a
// journal reader can match on them without parsing prose.
const (
	ReasonMaxParallel = "conditions: max-parallel"
	ReasonBudget      = "conditions: budget"
)

// budgetWindow is the rolling window MaxRunsPerHour is measured over.
const budgetWindow = time.Hour

// Conditions enforces run conditions (SCH-003) before a run starts: max
// concurrent runs per workflow, and a per-workflow run budget over a rolling
// hour. It never fails a tick — exhaustion means "skip this tick" (SCH-004
// backpressure), never an error. Safe for concurrent use: Admit is the atomic
// check-and-increment that makes "max-parallel holds under simultaneous ticks"
// true under real concurrency, not just sequential calls.
type Conditions struct {
	mu     sync.Mutex
	active map[string]int
	starts map[string][]time.Time
}

// NewConditions returns an empty Conditions tracker.
func NewConditions() *Conditions {
	return &Conditions{active: map[string]int{}, starts: map[string][]time.Time{}}
}

// Reconcile sets the initial active-run counts after a restart (Conditions'
// in-memory counters don't survive one) — see ActiveRunCounts.
func (c *Conditions) Reconcile(active map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for wf, n := range active {
		c.active[wf] = n
	}
}

// Admit atomically checks whether a new run of workflow may start under r at
// now and, if so, reserves the slot (increments the active count and records
// the start for the budget window) in the same critical section — the
// check-and-reserve is one atomic operation, which is what makes max-parallel
// hold under simultaneous ticks. A reserved admission MUST be paired with a
// later Release once the run finishes.
func (c *Conditions) Admit(workflow string, r apiv1.ReadinessConditions, now time.Time) (ok bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	maxConcurrent := r.MaxConcurrentRuns
	if maxConcurrent <= 0 {
		maxConcurrent = 1 // spec default (ReadinessConditions.MaxConcurrentRuns)
	}
	if c.active[workflow] >= int(maxConcurrent) {
		return false, ReasonMaxParallel
	}

	if r.MaxRunsPerHour > 0 {
		starts := pruneStarts(c.starts[workflow], now)
		c.starts[workflow] = starts
		if len(starts) >= int(r.MaxRunsPerHour) {
			return false, ReasonBudget
		}
	}

	c.active[workflow]++
	c.starts[workflow] = append(c.starts[workflow], now)
	return true, ""
}

// Release returns a workflow's admitted slot once its run finishes.
func (c *Conditions) Release(workflow string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[workflow] > 0 {
		c.active[workflow]--
	}
}

// Active reports the current active-run count for workflow (test/inspection).
func (c *Conditions) Active(workflow string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[workflow]
}

// pruneStarts drops start times older than budgetWindow before now.
func pruneStarts(starts []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-budgetWindow)
	i := 0
	for i < len(starts) && starts[i].Before(cutoff) {
		i++
	}
	if i == 0 {
		return starts
	}
	return append([]time.Time(nil), starts[i:]...)
}
