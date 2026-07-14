package localscheduler

import (
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Reason strings for a skipped tick (SCH-004 backpressure) — stable, so a
// journal reader can match on them without parsing prose.
const (
	ReasonMaxParallel         = "conditions: max-parallel"
	ReasonInstanceMaxParallel = "conditions: instance max-parallel"
	ReasonBudget              = "conditions: budget"
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

	// totalActive is the sum of active across every workflow — kept as a
	// running counter (not recomputed from active on every Admit) so Admit
	// stays O(1) regardless of workflow count.
	totalActive int
	// instanceMaxParallel caps totalActive across the whole instance (§7,
	// SCH-003's "per workflow/instance"); 0 means unlimited (unset).
	instanceMaxParallel int
	// workflowBudgets overrides a specific workflow's runs-per-hour budget
	// from instance.yaml's runConditions.workflowBudgets, taking precedence
	// over that workflow's own spec'd MaxRunsPerHour when set.
	workflowBudgets map[string]int
}

// NewConditions returns an empty Conditions tracker.
func NewConditions() *Conditions {
	return &Conditions{active: map[string]int{}, starts: map[string][]time.Time{}}
}

// SetInstanceLimits applies instance-level run conditions (instance.yaml's
// runConditions, §7/SCH-003) on top of each workflow's own per-workflow
// conditions: maxParallelRuns caps total concurrent runs across every
// workflow in the instance (0 = unlimited); workflowBudgets overrides a named
// workflow's runs-per-hour budget. Call once, before Admit is first used —
// it does not itself re-check already-admitted slots.
func (c *Conditions) SetInstanceLimits(maxParallelRuns int, workflowBudgets map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instanceMaxParallel = maxParallelRuns
	c.workflowBudgets = workflowBudgets
}

// Reconcile sets the initial active-run counts after a restart (Conditions'
// in-memory counters don't survive one) — see ActiveRunCounts. A seeded
// count MUST be paired with a later Release once whatever the daemon does
// with that pre-existing run (e.g. Runner.Resume, issue #135) finishes —
// Reconcile only seeds the starting point, exactly like Admit's own
// reserve-then-Release contract.
func (c *Conditions) Reconcile(active map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for wf, n := range active {
		c.active[wf] = n
	}
	total := 0
	for _, n := range c.active {
		total += n
	}
	c.totalActive = total
}

// ReconcileBudget seeds each workflow's rolling MaxRunsPerHour window from
// admitted-run start times read from durable history (the instance
// journal's run.started events) — issue #135's "budget amnesia": without
// this, Admit's in-memory starts map begins empty on every restart, so a
// crash-looping daemon admits one extra catch-up fire per restart, silently
// exceeding MaxRunsPerHour. Only entries within budgetWindow of now matter;
// Admit's own pruneStarts drops the rest lazily on first use, but callers
// may filter before calling this too.
func (c *Conditions) ReconcileBudget(starts map[string][]time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for wf, ts := range starts {
		c.starts[wf] = append([]time.Time(nil), ts...)
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
	if c.instanceMaxParallel > 0 && c.totalActive >= c.instanceMaxParallel {
		return false, ReasonInstanceMaxParallel
	}

	maxRunsPerHour := r.MaxRunsPerHour
	if override, ok := c.workflowBudgets[workflow]; ok && override > 0 {
		maxRunsPerHour = int32(override)
	}
	if maxRunsPerHour > 0 {
		starts := pruneStarts(c.starts[workflow], now)
		if len(starts) >= int(maxRunsPerHour) {
			c.starts[workflow] = starts
			return false, ReasonBudget
		}
		c.starts[workflow] = append(starts, now)
	}
	// A workflow with no effective budget never has c.starts[workflow]
	// touched, so it can't accumulate unboundedly (e.g. ~1,440 entries/day
	// for an `@every 1m` schedule) — nothing ever reads that map without a
	// budget check, so there's no reason to record starts for it.

	c.active[workflow]++
	c.totalActive++
	return true, ""
}

// Release returns a workflow's admitted slot once its run finishes.
func (c *Conditions) Release(workflow string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[workflow] > 0 {
		c.active[workflow]--
		c.totalActive--
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
