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
	// ReasonMemoryPressure prefixes the refusal of a new run because the
	// daemon's own memory cgroup is near its limit (#3949). Like
	// ReasonProviderQuota it is a stable, grep-able prefix with the
	// measurement appended, so the journal records not just that a run was
	// held back but what the cgroup looked like when it was. The condition is
	// transient: it clears on its own as runs finish and the kernel reclaims.
	ReasonMemoryPressure = "conditions: memory-pressure"
	// ReasonMissingCapability prefixes a schedule-time capability-match skip's
	// Reason (RRQ-1/#1101). Like ReasonProviderQuota it is a stable, grep-able
	// prefix, not a fixed string: dispatch appends the missing capability names
	// after it so the diagnostic — in the journal and in a caller-facing
	// TriggerRejectedError — names exactly what the runner failed to claim.
	// Unlike the capacity reasons this refusal is permanent for the running
	// config (a runner's claimed set is static), so it must not be treated as
	// transient.
	ReasonMissingCapability = "conditions: missing-capability"
	// ReasonPlacementUnsatisfiable prefixes the refusal of a workflow the
	// boot-time constraint solve marked unplaceable on the declared runners:
	// inventory (dsl-3.0.md §5 checkpoint 3, #2860: the workflow is refused
	// per-run with a named diagnostic; the daemon and every other workflow
	// keep serving). Like ReasonMissingCapability it is a stable prefix with
	// the solver's diagnostic appended, and the refusal is permanent for the
	// pinned inventory (restart-only, accept-and-pin — decision record D9).
	ReasonPlacementUnsatisfiable = "conditions: placement-unsatisfiable"
	// ReasonProviderQuota prefixes a provider-quota skip's Reason (#712).
	// Unlike the other Reason consts above (fixed strings), Admit appends the
	// resume time after this prefix — the acceptance criteria's own phrasing
	// ("journals tick.skipped reason:provider-quota") wants a stable,
	// grep-able prefix, and `goobers status` (a separate process invocation
	// that can't read the daemon's in-memory ProviderQuotaState directly)
	// needs the resume time recoverable from the journal alone.
	ReasonProviderQuota = "provider-quota"
	// ReasonProviderAuth identifies a workflow whose provider credential needs
	// operator repair. The scheduler does not retry this permanent condition.
	ReasonProviderAuth = "provider-auth-failed"
)

// OpenPRCounter reports the most recently polled count of a gaggle-scoped
// workflow's own un-merged PRs, matched under that gaggle's configured run-branch
// namespace (#1115). Admit reads it synchronously (a cheap in-memory read) to
// enforce ReadinessConditions.
// MaxOpenPRs (#353) without making a network call under the tick loop's lock —
// the count is refreshed on a separate background interval (openprcount.go).
// known is false on cold start (no poll completed yet) or after a poll error;
// Admit fails OPEN in that case — a transient GitHub hiccup must not stall
// dispatch, matching every other condition's "never fails a tick" contract.
type OpenPRCounter interface {
	OpenPRCount(gaggle, workflow string) (n int, known bool)
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
	// SCH-003's "per workflow/instance"); 0 means unlimited (unset). NOTE the
	// asymmetry with maxRunsPerHour just below in AdmitProviderWorkflow: that
	// field's 0/unset instead falls back to a non-zero default (10), never
	// "unlimited" (#3360) — two adjacent concurrency knobs in this same
	// struct where zero means opposite extremes.
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
	// memory backs the cgroup-aware admission gate (#3949): nil means no gate
	// is wired, so it is never enforced (fail-open), like openPRs and
	// providerQuota above. Read under c.mu in Admit; its UnderPressure is a
	// cached in-memory read with its own lock — no file read per admission.
	memory MemoryGate
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

// SetMemoryGate wires the cgroup-aware admission gate (#3949). Call once at
// setup, before Admit is first used. Nil (the default) leaves it unenforced.
func (c *Conditions) SetMemoryGate(gate MemoryGate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memory = gate
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
	return c.AdmitProviderWorkflow(identity, apiv1.ProviderGitHub, r, now)
}

// AdmitProviderWorkflow applies run conditions using the provider the run
// targets.
func (c *Conditions) AdmitProviderWorkflow(identity WorkflowIdentity, provider apiv1.Provider, r apiv1.ReadinessConditions, now time.Time) (ok bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Provider-quota circuit breaker (#712), checked first: while the provider
	// this run targets is known-exhausted, dispatching is pure waste regardless
	// of what the parallelism/budget checks below would otherwise allow. No
	// claim/budget/parallelism slot is reserved for a quota-skipped tick.
	if c.providerQuota != nil {
		if resetAt, exhausted := c.providerQuota.ExhaustedFor(provider, now); exhausted {
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
		if n, known := c.openPRs.OpenPRCount(identity.Gaggle, identity.Workflow); known && n >= int(r.MaxOpenPRs) {
			return false, ReasonOpenPRCap
		}
	}

	// #3439: an instance-config per-workflow budget override of exactly zero
	// means "stop this workflow from starting" — api/schemas/instance.schema.json
	// says so for both maps ("Zero stops it from starting"). The overrides below
	// are applied only when > 0, so a zero override was indistinguishable from
	// an absent one and fell through to the workflow's own value or the
	// scheduler default of 10. An operator writing `workflowBudgets: {wf: 0}` to
	// pause a workflow got ten runs an hour instead: the documented behaviour and
	// the actual behaviour were opposites, and the config validated clean, so the
	// only way to discover it was to watch the workflow run.
	//
	// Handled here rather than by relaxing the `> 0` guards because zero is not a
	// budget value in this scheme — it is a stop, and the two maps express it at
	// different windows. Note this is deliberately NOT the same question as the
	// workflow's own maxRunsPerHour/maxRunsPerDay fields, whose schema documents
	// zero as "fall back to the default of 10" and "disables the daily cap"
	// respectively; those already agree with the code and are left alone (#3360).
	if override, ok := c.workflowBudgets[identity.Workflow]; ok && override == 0 {
		return false, ReasonBudget
	}
	if override, ok := c.dayBudgets[identity.Workflow]; ok && override == 0 {
		return false, ReasonDailyBudget
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
		//
		// UNLIKE c.instanceMaxParallel above (where 0/unset means unlimited),
		// 0 here means the same thing as leaving the field unset: there is
		// no way to express "no hourly budget" through this field. An
		// operator who writes maxRunsPerHour: 0 expecting to mirror
		// maxParallelRuns's "0 = unlimited" instead gets silently throttled
		// to 10/hour (#3360) — `goobers validate` warns on this explicit-0
		// case (api/validate WF020).
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
	// Last, and only for new starts: the container-wide memory check (#3949).
	// It runs after the configured caps because it is not policy — it is the
	// physical resource every admitted run shares. Refusing here means the
	// operator's own limits would have allowed this run and the machine would
	// not. Deliberately not applied in ReserveContinuation: an already-started
	// run holds checkpoints and disk, and refusing to resume it strands that
	// work without freeing the memory an in-flight run has already charged.
	// Shedding *new* load while letting existing runs drain is what actually
	// lowers the ceiling.
	if c.memory != nil {
		if pressured, detail := c.memory.UnderPressure(); pressured {
			reason := ReasonMemoryPressure
			if detail != "" {
				reason += ": " + detail
			}
			return false, reason
		}
	}
	c.starts[identity] = append(starts, now)

	c.active[identity]++
	c.totalActive++
	return true, ""
}

// ReserveContinuation reacquires concurrency capacity for an existing run.
// Resuming a run is not a new start, so it does not consume rolling run
// budgets or re-evaluate start-only provider and open-PR gates.
func (c *Conditions) ReserveContinuation(identity WorkflowIdentity, r apiv1.ReadinessConditions) (ok bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	maxConcurrent := r.MaxConcurrentRuns
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if c.active[identity] >= int(maxConcurrent) {
		return false, ReasonMaxParallel
	}
	if c.instanceMaxParallel > 0 && c.totalActive >= c.instanceMaxParallel {
		return false, ReasonInstanceMaxParallel
	}
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
