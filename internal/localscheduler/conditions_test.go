package localscheduler

import (
	"strings"
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

func TestWorkerAvailabilityDistinguishesUnobservableFromZero(t *testing.T) {
	c := NewConditions()
	if available, observable := c.WorkerAvailability(); observable || available != 0 {
		t.Fatalf("unconfigured availability = (%d, %t), want unobservable", available, observable)
	}

	c.SetInstanceLimits(1, nil, nil)
	if available, observable := c.WorkerAvailability(); !observable || available != 1 {
		t.Fatalf("empty configured availability = (%d, %t), want 1 observable", available, observable)
	}
	if ok, reason := c.Admit("wf", apiv1.ReadinessConditions{MaxConcurrentRuns: 2}, time.Now()); !ok {
		t.Fatalf("Admit = false: %s", reason)
	}
	if available, observable := c.WorkerAvailability(); !observable || available != 0 {
		t.Fatalf("saturated configured availability = (%d, %t), want zero observable", available, observable)
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

// TestAdmitDailyBudgetCapsBelowHourlyBudget is #340's acceptance scenario:
// a workflow with MaxRunsPerHour:20, MaxRunsPerDay:2 admits at most 2 runs
// across 24h even though the hourly budget alone would allow far more —
// before this field existed, a daily ceiling could only be faked by
// combining a specific cron cadence with the hourly cap.
func TestAdmitDailyBudgetCapsBelowHourlyBudget(t *testing.T) {
	c := NewConditions()
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 20, MaxRunsPerDay: 2}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	ok, _ := c.Admit("wf", r, base)
	if !ok {
		t.Fatal("1st run should admit under the daily budget of 2")
	}
	c.Release("wf")
	// An hour later: well within the hourly budget of 20, still within the
	// daily budget of 2.
	ok, _ = c.Admit("wf", r, base.Add(time.Hour))
	if !ok {
		t.Fatal("2nd run (1h later) should admit under the daily budget of 2")
	}
	c.Release("wf")
	// Another hour later: the hourly budget (20) has plenty of headroom,
	// but the daily budget (2) is exhausted.
	ok, reason := c.Admit("wf", r, base.Add(2*time.Hour))
	if ok || reason != ReasonDailyBudget {
		t.Fatalf("3rd run should be refused by the daily budget despite hourly headroom: ok=%v reason=%s", ok, reason)
	}

	// Outside the rolling 24h window, the daily budget resets.
	ok, reason = c.Admit("wf", r, base.Add(25*time.Hour))
	if !ok {
		t.Fatalf("run outside the rolling daily window should admit: %s", reason)
	}
}

// TestWorkflowDailyBudgetOverridesPerWorkflowSpec mirrors
// TestWorkflowBudgetOverridesPerWorkflowSpec for the daily override
// (#340): instance.yaml's runConditions.workflowDailyBudgets overrides a
// specific workflow's runs-per-day budget without editing its own spec.
func TestWorkflowDailyBudgetOverridesPerWorkflowSpec(t *testing.T) {
	c := NewConditions()
	c.SetInstanceLimits(0, nil, map[string]int{"wf": 1})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 100, MaxRunsPerDay: 10} // spec allows 10/day

	ok, _ := c.Admit("wf", r, base)
	if !ok {
		t.Fatal("1st run should admit under the override daily budget of 1")
	}
	c.Release("wf")
	ok, reason := c.Admit("wf", r, base.Add(time.Minute))
	if ok || reason != ReasonDailyBudget {
		t.Fatalf("2nd run should be refused by the instance override (1/day), not the spec's 10/day: ok=%v reason=%s", ok, reason)
	}
}

// TestAdmitNoDailyBudgetMeansUncapped confirms MaxRunsPerDay's zero value
// has no non-zero spec default (unlike MaxRunsPerHour's #339 fallback) —
// a workflow that never sets it is bounded only by its (defaulted) hourly
// budget, not silently capped per day too.
func TestAdmitNoDailyBudgetMeansUncapped(t *testing.T) {
	c := NewConditions()
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 5} // no MaxRunsPerDay
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	admitted := 0
	for h := 0; h < 30; h++ { // 30 separate hourly windows, well over a day
		ok, _ := c.Admit("wf", r, base.Add(time.Duration(h)*time.Hour))
		if ok {
			admitted++
			c.Release("wf")
		}
	}
	if admitted != 30 {
		t.Fatalf("admitted = %d across 30 separate hourly windows, want 30 (no daily cap should apply)", admitted)
	}
}

// TestAdmitWithoutBudgetDefaultsToBoundedRate is #339's spec-default fix
// (a workflow with no MaxRunsPerHour configured now falls back to a sane
// default — 10/hour, see Admit — rather than "no budget enforced at all")
// applied to issue #138's original unbounded-growth concern: since Admit
// now always has an effective non-zero budget, it refuses once that budget
// is hit, which keeps Conditions' starts map bounded by construction —
// before #138's own fix (and still true after #339 changed what "no
// configured budget" defaults to), a schedule like `@every 1m` with no
// budget set could accumulate ~1,440 unpruned entries per day for the life
// of the daemon.
func TestAdmitWithoutBudgetDefaultsToBoundedRate(t *testing.T) {
	c := NewConditions()
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 1000} // no MaxRunsPerHour
	now := time.Now()

	admitted := 0
	for i := 0; i < 50; i++ {
		ok, reason := c.Admit("wf", r, now)
		if ok {
			admitted++
			c.Release("wf")
			continue
		}
		if reason != ReasonBudget {
			t.Fatalf("admit %d refused for %q, want budget", i, reason)
		}
	}
	if admitted != 10 {
		t.Fatalf("admitted = %d, want exactly 10 (the new spec default)", admitted)
	}

	c.mu.Lock()
	got := len(c.starts[WorkflowIdentity{Workflow: "wf"}])
	c.mu.Unlock()
	if got != 10 {
		t.Fatalf("starts[wf] = %d entries, want exactly 10 — bounded by the default budget, not unbounded", got)
	}
}

func TestDisableCadenceBudgetsWithReconciledHistory(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		readiness   apiv1.ReadinessConditions
		hourly      map[string]int
		daily       map[string]int
		wantBlocked string
	}{
		{name: "default hourly", wantBlocked: ReasonBudget},
		{name: "workflow hourly", readiness: apiv1.ReadinessConditions{MaxRunsPerHour: 2}, wantBlocked: ReasonBudget},
		{name: "workflow daily", readiness: apiv1.ReadinessConditions{MaxRunsPerHour: 100, MaxRunsPerDay: 2}, wantBlocked: ReasonDailyBudget},
		{name: "instance hourly", hourly: map[string]int{"wf": 2}, wantBlocked: ReasonBudget},
		{name: "instance daily", readiness: apiv1.ReadinessConditions{MaxRunsPerHour: 100}, daily: map[string]int{"wf": 2}, wantBlocked: ReasonDailyBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, disabled := range []bool{false, true} {
				t.Run(map[bool]string{false: "bounded", true: "unthrottled"}[disabled], func(t *testing.T) {
					c := NewConditions()
					c.SetInstanceLimits(1, tc.hourly, tc.daily)
					c.SetCadenceBudgetsDisabled(disabled)
					history := make([]time.Time, 24)
					for i := range history {
						history[i] = now.Add(-time.Duration(len(history)-i) * time.Minute)
						if i < 12 {
							history[i] = history[i].Add(-2 * time.Hour)
						}
					}
					c.ReconcileBudget(map[string][]time.Time{"wf": history})
					for i := 0; i < 12; i++ {
						ok, reason := c.Admit("wf", tc.readiness, now)
						if !disabled {
							if ok || reason != tc.wantBlocked {
								t.Fatalf("bounded admit = %v, %q; want %q", ok, reason, tc.wantBlocked)
							}
							return
						}
						if !ok {
							t.Fatalf("unthrottled admit %d: %s", i, reason)
						}
						c.Release("wf")
					}
					if got := len(c.starts[WorkflowIdentity{Workflow: "wf"}]); got != 36 {
						t.Fatalf("recorded starts = %d, want all 24 prior and 12 new admissions", got)
					}
					c.SetCadenceBudgetsDisabled(false)
					if ok, reason := c.Admit("wf", tc.readiness, now); ok || reason != tc.wantBlocked {
						t.Fatalf("re-enabled ceilings = %v, %q; want %q", ok, reason, tc.wantBlocked)
					}
				})
			}
		})
	}
}

func TestDisableCadenceBudgetsPreservesOtherConditions(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		setup func(*Conditions)
		want  string
	}{
		{"workflow concurrency", func(c *Conditions) { c.Reconcile(map[string]int{"wf": 1}) }, ReasonMaxParallel},
		{"instance concurrency", func(c *Conditions) {
			c.SetInstanceLimits(1, nil, nil)
			c.Reconcile(map[string]int{"other": 1})
		}, ReasonInstanceMaxParallel},
		{"hourly stop", func(c *Conditions) { c.SetInstanceLimits(0, map[string]int{"wf": 0}, nil) }, ReasonBudget},
		{"daily stop", func(c *Conditions) { c.SetInstanceLimits(0, nil, map[string]int{"wf": 0}) }, ReasonDailyBudget},
		{"provider quota", func(c *Conditions) {
			quota := NewProviderQuotaState()
			quota.RecordExhausted(now.Add(time.Hour))
			c.SetProviderQuota(quota)
		}, ReasonProviderQuota},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConditions()
			c.SetCadenceBudgetsDisabled(true)
			tc.setup(c)
			ok, reason := c.Admit("wf", apiv1.ReadinessConditions{}, now)
			if ok || !strings.HasPrefix(reason, tc.want) {
				t.Fatalf("admit = %v, %q; want refusal %q", ok, reason, tc.want)
			}
		})
	}
}

// TestInstanceMaxParallelCapsAcrossWorkflows is issue #142: instance.yaml's
// runConditions.maxParallelRuns was parsed and scaffolded but enforced
// nowhere — each workflow's own MaxConcurrentRuns capped that workflow
// alone, with no ceiling on the instance's total concurrent runs across
// every workflow combined (ARCHITECTURE §7's "max-parallel per
// workflow/instance").
func TestInstanceMaxParallelCapsAcrossWorkflows(t *testing.T) {
	c := NewConditions()
	c.SetInstanceLimits(2, nil, nil)
	now := time.Now()
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 5} // generous per-workflow limit

	ok, _ := c.Admit("wf-a", r, now)
	if !ok {
		t.Fatal("1st run should admit under the instance cap of 2")
	}
	ok, _ = c.Admit("wf-b", r, now)
	if !ok {
		t.Fatal("2nd run (different workflow) should admit under the instance cap of 2")
	}
	ok, reason := c.Admit("wf-c", r, now)
	if ok || reason != ReasonInstanceMaxParallel {
		t.Fatalf("3rd run should be refused by the instance-wide cap, not per-workflow: ok=%v reason=%s", ok, reason)
	}

	c.Release("wf-a")
	ok, reason = c.Admit("wf-c", r, now)
	if !ok {
		t.Fatalf("after a release, the instance cap should have room again: reason=%s", reason)
	}
}

// TestWorkflowBudgetOverridesPerWorkflowSpec is issue #142: instance.yaml's
// runConditions.workflowBudgets lets an operator override a specific
// workflow's runs-per-hour budget without editing that workflow's own spec —
// previously parsed and scaffolded but never consulted by Admit.
func TestWorkflowBudgetOverridesPerWorkflowSpec(t *testing.T) {
	c := NewConditions()
	c.SetInstanceLimits(0, map[string]int{"wf": 1}, nil)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 10} // spec allows 10/hr

	ok, _ := c.Admit("wf", r, base)
	if !ok {
		t.Fatal("1st run should admit under the override budget of 1")
	}
	c.Release("wf")
	ok, reason := c.Admit("wf", r, base.Add(time.Minute))
	if ok || reason != ReasonBudget {
		t.Fatalf("2nd run should be refused by the instance override (1/hr), not the spec's 10/hr: ok=%v reason=%s", ok, reason)
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

// #3439: api/schemas/instance.schema.json documents both per-workflow override
// maps as "Zero stops it from starting." The code applied an override only when
// it was > 0, so a zero was indistinguishable from an absent entry and fell
// through to the workflow's own value or the scheduler default of 10 -- the
// documented behaviour and the actual behaviour were exact opposites, and the
// config validated clean, so the only way to find out was to watch it run.
func TestZeroWorkflowBudgetOverrideStopsTheWorkflow(t *testing.T) {
	c := NewConditions()
	c.SetInstanceLimits(0, map[string]int{"wf": 0}, nil)
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 10}

	ok, reason := c.Admit("wf", r, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if ok {
		t.Fatal("a zero hourly override must stop the workflow, not fall back to the default of 10/hr")
	}
	if reason != ReasonBudget {
		t.Fatalf("reason = %q, want %q", reason, ReasonBudget)
	}

	ok, reason = c.admitProviderWorkflow(WorkflowIdentity{Workflow: "wf"}, apiv1.ProviderGitHub, r,
		time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC), true)
	if ok || reason != ReasonBudget {
		t.Fatalf("forced admission with zero hourly override: ok=%v reason=%q, want %q", ok, reason, ReasonBudget)
	}
}

func TestZeroDailyBudgetOverrideStopsTheWorkflow(t *testing.T) {
	c := NewConditions()
	c.SetInstanceLimits(0, nil, map[string]int{"wf": 0})
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 10}

	ok, reason := c.Admit("wf", r, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if ok {
		t.Fatal("a zero daily override must stop the workflow; previously it meant 'no daily cap', the opposite")
	}
	if reason != ReasonDailyBudget {
		t.Fatalf("reason = %q, want %q", reason, ReasonDailyBudget)
	}

	ok, reason = c.admitProviderWorkflow(WorkflowIdentity{Workflow: "wf"}, apiv1.ProviderGitHub, r,
		time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC), true)
	if ok || reason != ReasonDailyBudget {
		t.Fatalf("forced admission with zero daily override: ok=%v reason=%q, want %q", ok, reason, ReasonDailyBudget)
	}
}

// The stop is specific to an explicit zero. An absent override must still fall
// through to the spec value, or this fix would silently pause every workflow
// that simply has no override configured.
func TestAbsentBudgetOverrideStillFallsThroughToSpec(t *testing.T) {
	c := NewConditions()
	c.SetInstanceLimits(0, map[string]int{"other": 0}, map[string]int{"other": 0})
	r := apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 10}

	if ok, reason := c.Admit("wf", r, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); !ok {
		t.Fatalf("a workflow with no override of its own must still admit: ok=%v reason=%s", ok, reason)
	}
}
