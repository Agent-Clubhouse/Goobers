package gate

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

// The shared repass-budget transition (#3930), tested at the seam BOTH drivers
// call rather than through either one of them.
//
// internal/gate's evaluator suite already pins the behaviour end to end for the
// local runner (TestEvaluatorSeparatesInfrastructureAndPolicyRepassBudgets and
// friends), and internal/engine's parity rows pin it for the Temporal engine
// against the runner. These tests sit underneath both: they are the ones that
// say what the arithmetic IS, one property per test, so a future edit that
// changes it has to change a test that names the property it broke instead of
// a fixture that happens to notice.

// implementationLocalGate is the shipped shape the divergence actually bit:
// reference-workflows/gaggles/goobers/workflows/implementation.yaml's
// `local-gate`. A failure-class gate whose infra branch is a SELF SEND-BACK
// (local-ci re-runs the same CI against the same reviewed commit) and whose
// fail branch sends a DIFFERENT stage back (implement). The two targets being
// different stages is what makes the per-target cross-reset observable, and
// the infra branch repeating is what makes the bound observable.
func implementationLocalGate() apiv1.Gate {
	return apiv1.Gate{
		Name:      "local-gate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "failure-class"},
		Branches: map[string]string{
			OutcomePass:       "open-pr",
			OutcomeFail:       "implement",
			OutcomeInfra:      "local-ci",
			wf.BranchEscalate: "park-escalated",
		},
	}
}

// charge is the call both drivers make, with the fixture's re-entry answer
// wired in: every branch target in these tests except the pass branch is a
// stage that has already completed.
func charge(b *RepassBudget, g apiv1.Gate, outcome string, maxRepasses int) RepassCharge {
	target := g.Branches[outcome]
	return b.Charge(g, outcome, target, outcome != OutcomePass, maxRepasses)
}

// An infrastructure-only sequence is bounded by DefaultMaxInfrastructureRepasses
// (2), NOT by the policy budget (3 by default). This is difference (b) in
// #3930: the engine bounded it at the policy number, so a pure infrastructure
// flake got three retries there and two on the runner.
func TestRepassBudgetBoundsInfrastructureAtItsOwnLimit(t *testing.T) {
	g := implementationLocalGate()
	var b RepassBudget
	for attempt := 1; attempt <= DefaultMaxInfrastructureRepasses; attempt++ {
		c := charge(&b, g, OutcomeInfra, DefaultMaxRepasses)
		if c.Attempt != attempt || c.Exceeded || c.RepassTarget != "local-ci" {
			t.Fatalf("infrastructure repass %d = %+v, want an unescalated retry to local-ci", attempt, c)
		}
		if c.Bound != DefaultMaxInfrastructureRepasses {
			t.Fatalf("infrastructure repass %d bound = %d, want %d — the policy budget (%d) is a different number "+
				"and charging against it is the #3930 defect", attempt, c.Bound, DefaultMaxInfrastructureRepasses, DefaultMaxRepasses)
		}
	}
	exhausted := charge(&b, g, OutcomeInfra, DefaultMaxRepasses)
	if !exhausted.Exceeded || exhausted.Attempt != DefaultMaxInfrastructureRepasses+1 {
		t.Fatalf("infrastructure repass %d = %+v, want exhaustion", DefaultMaxInfrastructureRepasses+1, exhausted)
	}
	if got := exhausted.EscalationReason(); got != ReasonInfrastructureBudgetExhausted {
		t.Fatalf("escalation reason = %q, want %q", got, ReasonInfrastructureBudgetExhausted)
	}
	if got := b.RepassAttempts["local-ci"]; got != 0 {
		t.Fatalf("policy repasses charged to local-ci = %d, want 0 — infrastructure retries must not spend the "+
			"budget a review loop needs", got)
	}
}

// The two budgets do not consume each other. Difference (a): the engine
// charged both classes to one counter, so an infrastructure retry earlier in
// the run permanently shortened the review loop that followed it.
func TestRepassBudgetKeepsPolicyAndInfrastructureIndependent(t *testing.T) {
	g := implementationLocalGate()
	var b RepassBudget
	// Two infrastructure retries: the whole infrastructure budget.
	charge(&b, g, OutcomeInfra, DefaultMaxRepasses)
	charge(&b, g, OutcomeInfra, DefaultMaxRepasses)
	// Now the content loop. It must still have its FULL policy budget.
	for attempt := 1; attempt <= DefaultMaxRepasses; attempt++ {
		c := charge(&b, g, OutcomeFail, DefaultMaxRepasses)
		if c.Attempt != attempt || c.Exceeded || c.RepassTarget != "implement" {
			t.Fatalf("policy repass %d after two infrastructure retries = %+v, want the full %d-repass budget",
				attempt, c, DefaultMaxRepasses)
		}
		if c.Bound != DefaultMaxRepasses || c.Infrastructure {
			t.Fatalf("policy repass %d = %+v, want the policy bound %d", attempt, c, DefaultMaxRepasses)
		}
	}
	if c := charge(&b, g, OutcomeFail, DefaultMaxRepasses); !c.Exceeded {
		t.Fatalf("policy repass %d = %+v, want exhaustion at the policy bound", DefaultMaxRepasses+1, c)
	}
}

// The per-TARGET infrastructure counter cross-resets, in both directions: a
// non-infra outcome returns the infrastructure retries the run spent, and an
// infrastructure outcome does not disturb the policy count it interleaves
// with. Difference (c): the engine never reset it at all.
func TestRepassBudgetCrossResetsPerTargetCounters(t *testing.T) {
	g := implementationLocalGate()
	var b RepassBudget
	charge(&b, g, OutcomeInfra, DefaultMaxRepasses)
	charge(&b, g, OutcomeInfra, DefaultMaxRepasses)
	if got := b.InfrastructureRepassAttempts["local-ci"]; got != DefaultMaxInfrastructureRepasses {
		t.Fatalf("infrastructure repasses = %d, want %d", got, DefaultMaxInfrastructureRepasses)
	}
	// A content failure intervenes: the run got past the machinery fault.
	policy := charge(&b, g, OutcomeFail, DefaultMaxRepasses)
	if policy.Attempt != 1 {
		t.Fatalf("first policy repass = %+v, want attempt 1", policy)
	}
	if got := b.InfrastructureRepassAttempts["local-ci"]; got != 0 {
		t.Fatalf("infrastructure repasses after an intervening policy outcome = %d, want 0 — the reset is keyed "+
			"by the gate's declared infra branch, not by the target just taken", got)
	}
	// ...and the infrastructure budget really is spendable again, in full.
	for attempt := 1; attempt <= DefaultMaxInfrastructureRepasses; attempt++ {
		if c := charge(&b, g, OutcomeInfra, DefaultMaxRepasses); c.Attempt != attempt || c.Exceeded {
			t.Fatalf("infrastructure repass %d after the reset = %+v, want a fresh budget", attempt, c)
		}
	}
	// The policy counter was never touched by any of it.
	if got := b.RepassAttempts["implement"]; got != 1 {
		t.Fatalf("policy repasses for implement = %d, want 1 — infrastructure retries neither charge nor reset "+
			"the policy budget", got)
	}
	// A pass resets neither per-target budget: they are cumulative for the
	// whole run, because a stage that passes and is sent back again later has
	// still been sent back that many times.
	charge(&b, g, OutcomePass, DefaultMaxRepasses)
	if got := b.RepassAttempts["implement"]; got != 1 {
		t.Fatalf("policy repasses for implement after a pass = %d, want 1", got)
	}
}

// The per-GATE counters are a different pair from the per-TARGET ones, and
// they behave differently: they cross-reset on every class change and on a
// pass, because "three consecutive review failures" stops being true the
// moment the machinery is what failed.
func TestRepassBudgetSeparatesGateCountersFromTargetCounters(t *testing.T) {
	g := implementationLocalGate()
	var b RepassBudget
	first := charge(&b, g, OutcomeFail, DefaultMaxRepasses)
	second := charge(&b, g, OutcomeFail, DefaultMaxRepasses)
	if first.GateAttempt != 1 || second.GateAttempt != 2 {
		t.Fatalf("gate attempts = %d,%d, want 1,2", first.GateAttempt, second.GateAttempt)
	}
	infra := charge(&b, g, OutcomeInfra, DefaultMaxRepasses)
	if infra.GateAttempt != 1 {
		t.Fatalf("infrastructure gate attempt = %d, want 1 — the infrastructure class counts consecutively in "+
			"its own counter", infra.GateAttempt)
	}
	if got := b.Attempts[g.Name]; got != 0 {
		t.Fatalf("policy gate attempts after an infrastructure outcome = %d, want 0", got)
	}
	if got := b.RepassAttempts["implement"]; got != 2 {
		t.Fatalf("policy repasses for implement = %d, want 2 — the per-TARGET budget is cumulative and is NOT "+
			"reset by the per-gate cross-reset", got)
	}
	back := charge(&b, g, OutcomeFail, DefaultMaxRepasses)
	if back.GateAttempt != 1 || b.InfrastructureAttempts[g.Name] != 0 {
		t.Fatalf("policy outcome after infrastructure = %+v, infraGateAttempts=%d, want both cross-reset",
			back, b.InfrastructureAttempts[g.Name])
	}
	if back.Attempt != 3 {
		t.Fatalf("policy repass = %d, want 3 — the target budget continues across the class change", back.Attempt)
	}
	pass := charge(&b, g, OutcomePass, DefaultMaxRepasses)
	if pass.GateAttempt != 0 || b.Attempts[g.Name] != 0 || b.InfrastructureAttempts[g.Name] != 0 {
		t.Fatalf("pass = %+v, gate counters = %d/%d, want every per-gate counter cleared",
			pass, b.Attempts[g.Name], b.InfrastructureAttempts[g.Name])
	}
}

// Two gates sending the SAME stage back share that stage's budget, while their
// per-gate counters stay their own. This is why the budget is keyed by target:
// two gates bouncing one stage four times is the same non-convergence as one
// gate doing it four times.
func TestRepassBudgetSharesTargetBudgetAcrossGates(t *testing.T) {
	review := apiv1.Gate{Name: "review", Branches: map[string]string{
		OutcomePass: "local-ci", OutcomeFail: "implement"}}
	localGate := implementationLocalGate()
	var b RepassBudget
	if c := charge(&b, review, OutcomeFail, 2); c.Attempt != 1 || c.Exceeded {
		t.Fatalf("review repass = %+v, want attempt 1", c)
	}
	if c := charge(&b, localGate, OutcomeFail, 2); c.Attempt != 2 || c.Exceeded {
		t.Fatalf("local-gate repass = %+v, want attempt 2 on the SHARED implement budget", c)
	}
	if c := charge(&b, review, OutcomeFail, 2); !c.Exceeded {
		t.Fatalf("third repass to implement = %+v, want exhaustion of the shared budget", c)
	}
	if b.Attempts["review"] != 2 || b.Attempts["local-gate"] != 1 {
		t.Fatalf("per-gate attempts = review:%d local-gate:%d, want 2 and 1 — the gate counters are per gate even "+
			"where the budget is shared", b.Attempts["review"], b.Attempts["local-gate"])
	}
}

// A forward branch — a target that has NOT completed — charges nothing, in
// either class, while still advancing the gate's own class counter.
func TestRepassBudgetChargesNothingOnAForwardBranch(t *testing.T) {
	g := implementationLocalGate()
	var b RepassBudget
	c := b.Charge(g, OutcomeInfra, "park-infrastructure-failure", false, DefaultMaxRepasses)
	if c.Attempt != 0 || c.RepassTarget != "" || c.Exceeded {
		t.Fatalf("forward infrastructure branch = %+v, want no charge", c)
	}
	if c.GateAttempt != 1 {
		t.Fatalf("forward branch gate attempt = %d, want 1", c.GateAttempt)
	}
	if len(b.InfrastructureRepassAttempts) != 0 || len(b.RepassAttempts) != 0 {
		t.Fatalf("forward branch charged a budget: infra=%v policy=%v", b.InfrastructureRepassAttempts, b.RepassAttempts)
	}
}

// The per-gate leaf override is resolved inside Charge, so both drivers apply
// it identically — and it applies to the POLICY budget only. The
// infrastructure bound is a constant no definition can widen: a gate that
// wanted more content repasses did not ask for more retries against a broken
// runner host.
func TestRepassBudgetAppliesTheGateLeafOverrideToPolicyOnly(t *testing.T) {
	g := implementationLocalGate()
	g.MaxRepasses = 5
	var b RepassBudget
	if c := charge(&b, g, OutcomeFail, 1); c.Bound != 5 || c.Exceeded {
		t.Fatalf("policy repass under a gate override = %+v, want bound 5", c)
	}
	if c := charge(&b, g, OutcomeInfra, 1); c.Bound != DefaultMaxInfrastructureRepasses {
		t.Fatalf("infrastructure repass under a gate override = %+v, want bound %d",
			c, DefaultMaxInfrastructureRepasses)
	}
}

// Every counter is zero-value usable, and a budget carrying only the counters
// an OLD history could reconstruct behaves as if the missing ones are zero.
//
// This is the backwards-compatibility contract for both drivers: the engine
// replays a history recorded before the infrastructure counters existed, and
// the runner resumes a journal whose gate.evaluated events carry no
// infrastructure repassTarget. Neither can produce a value for a counter that
// was never journaled, and the correct reading of that absence is "no
// infrastructure repass has been charged" — not a panic on a nil map, and not
// a budget that starts already spent.
func TestRepassBudgetIsZeroValueUsableForOldHistories(t *testing.T) {
	g := implementationLocalGate()
	var fresh RepassBudget
	if c := charge(&fresh, g, OutcomeInfra, DefaultMaxRepasses); c.Attempt != 1 || c.Exceeded {
		t.Fatalf("first charge on a zero-value budget = %+v, want attempt 1", c)
	}
	// A resume that reconstructed only the policy counters an old journal
	// carries: the infrastructure maps stay nil and must read as 0.
	seeded := RepassBudget{
		Attempts:       map[string]int{"local-gate": 2},
		RepassAttempts: map[string]int{"implement": 2},
	}
	c := charge(&seeded, g, OutcomeInfra, DefaultMaxRepasses)
	if c.Attempt != 1 || c.GateAttempt != 1 || c.Exceeded {
		t.Fatalf("infrastructure charge against policy-only seeds = %+v, want a fresh infrastructure budget", c)
	}
	if got := seeded.RepassAttempts["implement"]; got != 2 {
		t.Fatalf("seeded policy budget = %d, want the resumed 2 left intact", got)
	}
}

// A budget re-seeded from what a journal/history preserves reaches the SAME
// decision as the run that was interrupted: replaying the sequence continues
// the budget instead of handing the run a fresh one, and the escalation lands
// on the same evaluation either way.
//
// The seeds are exactly the four numbers both drivers can recover — the runner
// from repassTarget/repassAttempt/gateAttempt on each gate.evaluated event,
// the engine from a deterministic replay of the same outcome sequence.
func TestRepassBudgetResumesToTheSameDecision(t *testing.T) {
	g := implementationLocalGate()
	sequence := []string{OutcomeInfra, OutcomeInfra, OutcomeFail, OutcomeInfra, OutcomeInfra, OutcomeInfra}

	var continuous RepassBudget
	var continuousCharges []RepassCharge
	for _, outcome := range sequence {
		continuousCharges = append(continuousCharges, charge(&continuous, g, outcome, DefaultMaxRepasses))
	}
	if !continuousCharges[len(continuousCharges)-1].Exceeded {
		t.Fatalf("uninterrupted walk did not exhaust the infrastructure budget: %+v", continuousCharges)
	}

	// Interrupt after the fourth outcome and resume from the counters the
	// journal preserved.
	var before RepassBudget
	for _, outcome := range sequence[:4] {
		charge(&before, g, outcome, DefaultMaxRepasses)
	}
	resumed := RepassBudget{
		Attempts:                     map[string]int{g.Name: before.Attempts[g.Name]},
		InfrastructureAttempts:       map[string]int{g.Name: before.InfrastructureAttempts[g.Name]},
		RepassAttempts:               map[string]int{"implement": before.RepassAttempts["implement"]},
		InfrastructureRepassAttempts: map[string]int{"local-ci": before.InfrastructureRepassAttempts["local-ci"]},
	}
	for i, outcome := range sequence[4:] {
		got := charge(&resumed, g, outcome, DefaultMaxRepasses)
		want := continuousCharges[4+i]
		if got != want {
			t.Fatalf("resumed charge %d = %+v, want the uninterrupted walk's %+v — a resume that loses the "+
				"infrastructure counters hands the run a budget it already spent", 5+i, got, want)
		}
	}
}

// Charge is deterministic: the same sequence over the same seeds produces the
// identical charges every time. The engine holds this budget as workflow state
// and Temporal re-executes the workflow function against a recorded history, so
// a transition that depended on map iteration order (or anything else
// unordered) would wedge a live run rather than fail a test.
func TestRepassBudgetChargeIsDeterministic(t *testing.T) {
	g := implementationLocalGate()
	sequence := []string{OutcomeFail, OutcomeInfra, OutcomeFail, OutcomeInfra, OutcomeInfra, OutcomePass, OutcomeFail}
	walk := func() []RepassCharge {
		var b RepassBudget
		var out []RepassCharge
		for _, outcome := range sequence {
			out = append(out, charge(&b, g, outcome, DefaultMaxRepasses))
		}
		return out
	}
	first, second := walk(), walk()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("charge %d differs between two identical walks:\n  first:  %+v\n  second: %+v",
				i+1, first[i], second[i])
		}
	}
}
