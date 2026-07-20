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
	ReasonDailyBudget         = "conditions: daily-budget"
	ReasonOpenPRCap           = "conditions: open-pr-cap"
	// ReasonProviderQuota prefixes a provider-quota skip's Reason (#712).
	// Unlike the other Reason consts above (fixed strings), Admit appends the
	// resume time after this prefix — the acceptance criteria's own phrasing
	// ("journals tick.skipped reason:provider-quota") wants a stable,
	// grep-able prefix, and `goobers status` (a separate process invocation
	// that can't read the daemon's in-memory ProviderQuotaState directly)
	// needs the resume time recoverable from the journal alone.
	ReasonProviderQuota = "provider-quota"
)

// OpenPRCounter reports the most recently polled count of a workflow's own
// un-merged PRs. Admit reads it synchronously (a cheap in-memory read) to enforce
// ReadinessConditions.
// MaxOpenPRs (#353) without making a network call under the tick loop's lock —
// the count is refreshed on a separate background interval (openprcount.go).
// known is false on cold start (no poll completed yet) or after a poll error;
// Admit fails OPEN in that case — a transient GitHub hiccup must not stall
// dispatch, matching every other condition's "never fails a tick" contract.
type OpenPRCounter interface {
	OpenPRCount(workflow string) (n int, known bool)
}

// budgetWindow is the rolling window MaxRunsPerHour is measured over.
const budgetWindow = time.Hour

// dayWindow is the rolling window MaxRunsPerDay is measured over (#340).
// Also the width Admit retains starts to: it's a strict superset of
// budgetWindow, so one starts-per-workflow history serves both the hourly
// and the daily check without a second tracked slice.
const dayWindow = 24 * time.Hour

// Conditions enforces run conditions (SCH-003) before a run starts: max
// concurrent runs per workflow, and a per-workflow run budget over a rolling
// hour. It never fails a tick — exhaustion means "skip this tick" (SCH-004
// backpressure), never an error. Safe for concurrent use: Admit is the atomic
// check-and-increment that makes "max-parallel holds under simultaneous ticks"
// true under real concurrency, not just sequential calls.
type Conditions struct {
	mu     sync.Mutex
	active map[WorkflowIdentity]int
	starts map[WorkflowIdentity][]time.Time

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
	// dayBudgets overrides a specific workflow's runs-per-day budget from
	// instance.yaml's runConditions.workflowDailyBudgets (#340), mirroring
	// workflowBudgets's precedence over the workflow's own spec'd
	// MaxRunsPerDay.
	dayBudgets map[string]int
	// openPRs backs the MaxOpenPRs cap (#353) with a cached count refreshed
	// off-tick; nil means no counter is wired, so the cap is never enforced
	// (fail-open). Read under c.mu in Admit, but its own OpenPRCount is a cheap
	// in-memory read with its own lock — no network call ever runs under c.mu.
	openPRs OpenPRCounter
	// providerQuota backs the provider-quota circuit breaker (#712): nil
	// means no gate is wired, so it's never enforced (fail-open), matching
	// every other condition's "never fails a tick on missing wiring"
	// contract. Read under c.mu in Admit; its own Exhausted is a cheap
	// in-memory read with its own lock — no network call ever runs under c.mu.
	providerQuota ProviderQuotaGate
}

// NewConditions returns an empty Conditions tracker.
func NewConditions() *Conditions {
	return &Conditions{
		active: map[WorkflowIdentity]int{},
		starts: map[WorkflowIdentity][]time.Time{},
	}
}

// SetInstanceLimits applies instance-level run conditions (instance.yaml's
// runConditions, §7/SCH-003) on top of each workflow's own per-workflow
// conditions: maxParallelRuns caps total concurrent runs across every
// workflow in the instance (0 = unlimited); workflowBudgets overrides a named
// workflow's runs-per-hour budget; dayBudgets overrides a named workflow's
// runs-per-day budget (#340). Call once, before Admit is first used — it
// does not itself re-check already-admitted slots.
func (c *Conditions) SetInstanceLimits(maxParallelRuns int, workflowBudgets map[string]int, dayBudgets map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instanceMaxParallel = maxParallelRuns
	c.workflowBudgets = workflowBudgets
	c.dayBudgets = dayBudgets
}

// SetOpenPRCounter wires the cached open-PR counter that backs the MaxOpenPRs
// cap (#353). Call once at setup, before Admit is first used. Nil (the default)
// leaves the cap unenforced.
func (c *Conditions) SetOpenPRCounter(counter OpenPRCounter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.openPRs = counter
}

// SetProviderQuota wires the gate that backs the provider-quota circuit
// breaker (#712). Call once at setup, before Admit is first used. Nil (the
// default) leaves the breaker unenforced.
func (c *Conditions) SetProviderQuota(gate ProviderQuotaGate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.providerQuota = gate
}

// Reconcile sets the initial active-run counts after a restart (Conditions'
// in-memory counters don't survive one) — see ActiveRunCounts. A seeded
// count MUST be paired with a later Release once whatever the daemon does
// with that pre-existing run (e.g. Runner.Resume, issue #135) finishes —
// Reconcile only seeds the starting point, exactly like Admit's own
// reserve-then-Release contract.
func (c *Conditions) Reconcile(active map[string]int) {
	scoped := make(map[WorkflowIdentity]int, len(active))
	for workflow, count := range active {
		scoped[WorkflowIdentity{Workflow: workflow}] = count
	}
	c.ReconcileWorkflows(scoped)
}

// ReconcileWorkflows sets initial active-run counts keyed by gaggle and
// workflow. Scheduler recovery uses this form so duplicate workflow names do
// not share concurrency slots.
func (c *Conditions) ReconcileWorkflows(active map[WorkflowIdentity]int) {
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

// ReconcileBudget seeds each workflow's rolling MaxRunsPerHour/MaxRunsPerDay
// window from admitted-run start times read from durable history (the
// instance journal's run.started events) — issue #135's "budget amnesia":
// without this, Admit's in-memory starts map begins empty on every restart,
// so a crash-looping daemon admits one extra catch-up fire per restart,
// silently exceeding the budget. Only entries within dayWindow of now
// matter (#340: widened from budgetWindow so the daily check also survives
// a restart) — Admit's own pruneStarts drops the rest lazily on first use,
// but callers may filter before calling this too.
func (c *Conditions) ReconcileBudget(starts map[string][]time.Time) {
	scoped := make(map[WorkflowIdentity][]time.Time, len(starts))
	for workflow, times := range starts {
		scoped[WorkflowIdentity{Workflow: workflow}] = times
	}
	c.ReconcileWorkflowBudgets(scoped)
}

// ReconcileWorkflowBudgets restores rolling budget history keyed by gaggle and
// workflow.
func (c *Conditions) ReconcileWorkflowBudgets(starts map[WorkflowIdentity][]time.Time) {
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
	return c.AdmitWorkflow(WorkflowIdentity{Workflow: workflow}, r, now)
}

// AdmitWorkflow applies run conditions to one gaggle-scoped workflow.
func (c *Conditions) AdmitWorkflow(identity WorkflowIdentity, r apiv1.ReadinessConditions, now time.Time) (ok bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Provider-quota circuit breaker (#712), checked first: while a provider
	// the run would need is known-exhausted, dispatching is pure waste
	// regardless of what the parallelism/budget checks below would otherwise
	// allow — every workflow today requires GitHub for its first stage (the
	// issue's own "all four" note), so this applies unconditionally rather
	// than per-workflow. No claim/budget/parallelism slot is reserved for a
	// quota-skipped tick (this returns before any of those increment).
	if c.providerQuota != nil {
		if resetAt, exhausted := c.providerQuota.Exhausted(now); exhausted {
			return false, ReasonProviderQuota + ": resumes at " + resetAt.UTC().Format(time.RFC3339)
		}
	}

	maxConcurrent := r.MaxConcurrentRuns
	if maxConcurrent <= 0 {
		maxConcurrent = 1 // spec default (ReadinessConditions.MaxConcurrentRuns)
	}
	if c.active[identity] >= int(maxConcurrent) {
		return false, ReasonMaxParallel
	}

	// Open-PR cap (#353): a workflow that opts in (MaxOpenPRs > 0) is throttled
	// once its own un-merged PRs reach the cap, so a loop outrunning human (or
	// V0.5 auto-) merge cadence can't accrete mutually-un-mergeable siblings.
	// A cheap in-memory read of the off-tick-refreshed count; fail-open when the
	// count is unknown (cold start / poll error) so a GitHub hiccup never stalls
	// a tick. Cross-PR rebase/conflict resolution stays V0.5's (epic #357).
	if r.MaxOpenPRs > 0 && c.openPRs != nil {
		if n, known := c.openPRs.OpenPRCount(identity.Workflow); known && n >= int(r.MaxOpenPRs) {
			return false, ReasonOpenPRCap
		}
	}

	maxRunsPerHour := r.MaxRunsPerHour
	if override, ok := c.workflowBudgets[identity.Workflow]; ok && override > 0 {
		maxRunsPerHour = int32(override)
	} else if maxRunsPerHour <= 0 {
		// spec default (ReadinessConditions.MaxRunsPerHour, #339): unset used
		// to mean "no hourly budget enforced at all" — a silent WF-015 gap,
		// since a workflow that never declares this field got zero
		// protection against a runaway emergent chain. 10/hour mirrors
		// MaxConcurrentRuns's own <= 0 fallback just above: a sane, non-zero
		// guardrail out of the box, generous enough that a single clean run
		// (completes in well under 10 minutes) doesn't get throttled the way
		// a hand-authored maxRunsPerHour: 1 did during dogfooding.
		maxRunsPerHour = 10
	}
	maxRunsPerDay := r.MaxRunsPerDay
	if override, ok := c.dayBudgets[identity.Workflow]; ok && override > 0 {
		maxRunsPerDay = int32(override)
	}

	// Retained at dayWindow width (a strict superset of budgetWindow) so one
	// starts history serves both checks (#340) — hourlyCount is a sub-count
	// of the same slice, not a second tracked list.
	starts := pruneStarts(c.starts[identity], now, dayWindow)
	hourlyCount := countSince(starts, now.Add(-budgetWindow))
	if hourlyCount >= int(maxRunsPerHour) {
		c.starts[identity] = starts
		return false, ReasonBudget
	}
	if maxRunsPerDay > 0 && len(starts) >= int(maxRunsPerDay) {
		c.starts[identity] = starts
		return false, ReasonDailyBudget
	}
	// Check the shared pool after every workflow-specific readiness condition,
	// so a pool refusal means this workflow was otherwise dispatchable.
	if c.instanceMaxParallel > 0 && c.totalActive >= c.instanceMaxParallel {
		return false, ReasonInstanceMaxParallel
	}
	c.starts[identity] = append(starts, now)

	c.active[identity]++
	c.totalActive++
	return true, ""
}

// Release returns a workflow's admitted slot once its run finishes.
func (c *Conditions) Release(workflow string) {
	c.ReleaseWorkflow(WorkflowIdentity{Workflow: workflow})
}

// ReleaseWorkflow returns a gaggle-scoped workflow's admitted slot.
func (c *Conditions) ReleaseWorkflow(identity WorkflowIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[identity] > 0 {
		c.active[identity]--
		c.totalActive--
	}
}

// Active reports the current active-run count for workflow (test/inspection).
func (c *Conditions) Active(workflow string) int {
	return c.ActiveWorkflow(WorkflowIdentity{Workflow: workflow})
}

// ActiveWorkflow reports the active-run count for a gaggle-scoped workflow.
func (c *Conditions) ActiveWorkflow(identity WorkflowIdentity) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[identity]
}

// pruneStarts drops start times older than window before now. starts is
// assumed sorted ascending (Admit only ever appends now, which advances
// monotonically call to call), so the retained tail is already sorted too.
func pruneStarts(starts []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	i := 0
	for i < len(starts) && starts[i].Before(cutoff) {
		i++
	}
	if i == 0 {
		return starts
	}
	return append([]time.Time(nil), starts[i:]...)
}

// countSince counts the tail of a sorted-ascending starts slice at or after
// cutoff — a narrower sub-window over the same slice pruneStarts already
// retained at a wider width (#340: one starts history serves both the
// hourly and the daily check without tracking a second slice).
func countSince(starts []time.Time, cutoff time.Time) int {
	n := 0
	for i := len(starts) - 1; i >= 0; i-- {
		if starts[i].Before(cutoff) {
			break
		}
		n++
	}
	return n
}
