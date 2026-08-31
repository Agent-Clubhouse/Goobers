package engine

// Parity rows E11-infra-repass-* — finding-002 inventory row "gate repass
// accounting: the infrastructure budget is separate from the policy budget, at
// its own bound, and both cross-reset" (#3930, decision 005 / #3828's
// checklist).
//
// # What the two runners disagreed about
//
// internal/gate.Evaluator.trackRepass keeps TWO repass budgets and keeps them
// apart. internal/engine's resolveGateOutcome kept ONE:
//
//	repassAttempts[target]++
//	attempt := repassAttempts[target]
//	escalated := attempt > runcontrol.MaxRepassesForGate(g, maxRepasses)
//
// `outcome` was in scope and never consulted. So on the engine an
// infrastructure repass (a) was charged to the same counter as content
// repasses to the same target, so the two budgets consumed each other; (b) was
// bounded by MaxRepassesForGate (3) instead of gate.DefaultMaxInfrastructureRepasses
// (2); and (c) was never reset when a non-infra outcome intervened, so an
// infrastructure retry from earlier in the run permanently consumed budget the
// local runner had returned. The per-gate counters collapsed the same way: the
// engine advanced one number for every non-pass regardless of class where the
// evaluator maintains and cross-resets two.
//
// # Why this table did not see it
//
// Both differences need a gate.OutcomeInfra outcome, which needs the
// `failure-class` evaluator — and every row here that reaches a retry arm uses
// `status-equals`, which cannot produce infra at all. The single row that does
// (E10-learning-episode-infra-forward-branch) routes its infra branch ONWARD,
// to a stage that has never run, so it charges no budget by construction and
// compares repassAttempt 0 against repassAttempt 0. Nothing in the table sent
// the same target back twice on an infrastructure outcome, which is the
// smallest sequence in which the two counters can differ.
//
// # The shape every row here uses
//
// reference-workflows/gaggles/goobers/workflows/implementation.yaml:418-427 —
// the SHIPPED lane, the second one finding 002 R11 schedules onto the engine:
//
//	- name: local-gate
//	  evaluator: automated
//	  automated:
//	    check: failure-class
//	  branches:
//	    pass: open-pr
//	    fail: implement          # content: send the implementer back
//	    infra: local-ci          # infrastructure: re-run CI on the same commit
//	    escalate: park-escalated
//
// The infra branch is a SELF SEND-BACK — local-ci re-entering itself — which is
// what makes the bound observable, and it is a DIFFERENT stage from the fail
// branch's target, which is what makes the per-target cross-reset observable. A
// row over "gate"/"target" would prove the mechanism against a shape no lane
// has; these rows keep the production names because the argument is about what
// those stages are.

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// implementationLocalGateSpec is the fixture shared by the first three rows:
// implement -> local-ci -> local-gate, with the shipped branch set and a
// parking stage on the escalate branch so an exhausted budget has somewhere to
// land that both sides can compare.
func implementationLocalGateSpec() apiv1.WorkflowSpec {
	return fixtureSpec("implement",
		[]apiv1.Task{
			detTask("implement", "local-ci"),
			detTask("local-ci", "local-gate"),
			detTask("park-escalated", wf.TargetEscalate),
		},
		[]apiv1.Gate{failureClassGate("local-gate", map[string]string{
			"pass":            wf.TerminalComplete,
			"fail":            "implement",
			"infra":           "local-ci",
			wf.BranchEscalate: "park-escalated",
		})},
	)
}

func init() {
	// --- the bound ------------------------------------------------------------
	//
	// An infrastructure-ONLY run. local-ci reports a retryable failure every
	// time, so local-gate answers infra every time and sends local-ci back into
	// itself. The infrastructure budget is 2, so the run gets two retries and
	// escalates on the third evaluation — three local-ci dispatches, not four.
	//
	// The engine's single budget was the POLICY one (3), so it granted a fourth
	// dispatch: an infrastructure-only run got 3 retries where the runner gives
	// it 2. That is a whole extra CI execution against a host that is already
	// failing, and a terminal that arrives one attempt later.
	registerParityRow(parityCase{
		Row:        rowInfraRepassBudgetBound,
		Name:       "an infrastructure-only sequence is bounded at 2, not at the policy budget",
		Lane:       "implementation.yaml",
		DSLVersion: "2.0",
		Spec:       implementationLocalGateSpec(),
		Script: map[string][]scriptedCall{
			"implement": {succeed(map[string]interface{}{"committed": "true"})},
			// Retryable is the producer's own claim that the machinery failed
			// rather than the work — the only input "failure-class" reads to
			// answer infra. Four are scripted for a budget of two plus the
			// evaluation that exhausts it: a side that takes a fourth dispatch
			// gets it, and is reported as the extra dispatch it is rather than
			// as an exhausted script.
			"local-ci": {
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
			},
			"park-escalated": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseInfraRepassBudgetBound,
		Check:   checkInfraRepassBudgetBound,
	})

	// --- independence and the reset, in both directions -----------------------
	//
	// Two infrastructure retries (the whole infrastructure budget), then a
	// content failure, then three more infrastructure outcomes. On the runner
	// the content failure returns the infrastructure budget, so the second
	// infrastructure sequence gets its full two retries and escalates on its
	// third — and the content failure itself started a policy budget that the
	// earlier infrastructure retries had not touched.
	//
	// The engine, holding one counter with no reset, was two ahead by the time
	// the second sequence started and escalated an evaluation earlier.
	registerParityRow(parityCase{
		Row:        rowInfraRepassBudgetIndependence,
		Name:       "policy and infrastructure budgets do not consume each other and the infra counter resets",
		Lane:       "implementation.yaml",
		DSLVersion: "2.0",
		Spec:       implementationLocalGateSpec(),
		Script: map[string][]scriptedCall{
			"implement": {
				succeed(map[string]interface{}{"committed": "true"}),
				succeed(map[string]interface{}{"committed": "true"}),
			},
			"local-ci": {
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				// The intervening CONTENT failure: unmarked, so failure-class
				// answers fail and the run sends the implementer back.
				fail("nonzero_exit", "unit tests failed"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
			},
			"park-escalated": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseInfraRepassBudgetIndependence,
		Check:   checkInfraRepassBudgetIndependence,
	})

	// --- per-gate counters vs per-target budgets ------------------------------
	//
	// The other axis of the same collapse, and the one that survives even where
	// the routing agrees. Two gates send the SAME stage back, so implement's
	// budget is shared between them while each gate's own evaluation counter
	// stays its own — and local-gate's counter cross-resets when the class
	// changes, which the engine's single counter did not.
	registerParityRow(parityCase{
		Row:        rowInfraRepassCounterSeparation,
		Name:       "per-gate evaluation counters cross-reset over a target budget two gates share",
		Lane:       "implementation.yaml",
		DSLVersion: "2.0",
		Spec: fixtureSpec("implement",
			[]apiv1.Task{
				detTask("implement", "local-ci"),
				detTask("local-ci", "local-gate"),
				detTask("verify", "verify-gate"),
				detTask("park-escalated", wf.TargetEscalate),
			},
			[]apiv1.Gate{
				failureClassGate("local-gate", map[string]string{
					"pass":            "verify",
					"fail":            "implement",
					"infra":           "local-ci",
					wf.BranchEscalate: "park-escalated",
				}),
				// The SECOND gate charging the same target. status-equals is
				// the ordinary evaluator over a stage result; it cannot answer
				// infra, which is the point — implement's budget is charged by
				// a gate that has no infrastructure branch at all.
				statusGate("verify-gate", map[string]string{
					"pass":            wf.TerminalComplete,
					"fail":            "implement",
					wf.BranchEscalate: "park-escalated",
				}),
			},
		),
		Script: map[string][]scriptedCall{
			"implement": {
				succeed(map[string]interface{}{"committed": "true"}),
				succeed(map[string]interface{}{"committed": "true"}),
				succeed(map[string]interface{}{"committed": "true"}),
				succeed(map[string]interface{}{"committed": "true"}),
			},
			"local-ci": {
				fail("nonzero_exit", "unit tests failed"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				fail("nonzero_exit", "unit tests failed"),
				succeed(map[string]interface{}{"ci": "green"}),
				succeed(map[string]interface{}{"ci": "green"}),
			},
			"verify": {
				fail("assertion_failed", "acceptance check failed"),
				fail("assertion_failed", "acceptance check failed"),
			},
			"park-escalated": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseInfraRepassCounterSeparation,
		Check:   checkInfraRepassCounterSeparation,
	})

	// --- resumed / replayed histories -----------------------------------------
	//
	// The durability half. Neither driver keeps its budget anywhere but in
	// memory: the runner reconstructs it from the journal on resume
	// (internal/runner/resume.go's targetRepassSeed / infrastructureTargetRepassSeed
	// / gateInfrastructureSeed), and the engine reconstructs it by replaying the
	// workflow function against its recorded history. Both reconstructions read
	// the SAME four numbers off gate.evaluated — outcome, repassTarget,
	// repassAttempt, gateAttempt — so a side that charges correctly in memory
	// but journals a collapsed number hands its own resume a budget that is
	// already spent, and the divergence comes back on the next interruption
	// rather than on the next release.
	//
	// The row rebuilds a gate.RepassBudget from each side's journal and diffs
	// the two, then charges one more evaluation of each class through the SHARED
	// helper to prove the two reconstructions also make the same next decision.
	registerParityRow(parityCase{
		Row:        rowInfraRepassBudgetResumed,
		Name:       "a resumed or replayed history rebuilds the same budgets and the same next decision",
		Lane:       "implementation.yaml",
		DSLVersion: "2.0",
		Spec:       implementationLocalGateSpec(),
		Script: map[string][]scriptedCall{
			"implement": {
				succeed(map[string]interface{}{"committed": "true"}),
				succeed(map[string]interface{}{"committed": "true"}),
			},
			"local-ci": {
				failRetryable("worktree_provision_failed", "runner host contention"),
				fail("nonzero_exit", "unit tests failed"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
				failRetryable("worktree_provision_failed", "runner host contention"),
			},
			"park-escalated": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseInfraRepassBudgetResumed,
		Check:   checkInfraRepassBudgetResumed,
	})
}

// --- observation ------------------------------------------------------------

// gateEvaluationRecord is one gate.evaluated event reduced to the repass
// accounting both drivers journal for it. The four Runner-namespace fields are
// the resume/replay contract: they are what internal/runner/resume.go reads to
// rebuild the budget, and (repassTarget aside) they are excluded from
// journal.ConformanceView, so a divergence in them is invisible to
// diffConformanceViews and has to be compared here.
type gateEvaluationRecord struct {
	Gate          string
	Outcome       string
	Target        string
	Escalated     bool
	RepassAttempt int
	GateAttempt   int
	RepassTarget  string
	Reason        string
}

func (r gateEvaluationRecord) String() string {
	return fmt.Sprintf("gate=%s outcome=%s target=%s escalated=%t repassAttempt=%d gateAttempt=%d repassTarget=%s reason=%s",
		r.Gate, r.Outcome, r.Target, r.Escalated, r.RepassAttempt, r.GateAttempt, r.RepassTarget, r.Reason)
}

// gateEvaluations extracts a side's gate.evaluated events in order.
func gateEvaluations(side paritySide) []gateEvaluationRecord {
	var out []gateEvaluationRecord
	for _, e := range side.Events {
		if e.Type != journal.EventGateEvaluated {
			continue
		}
		rec := gateEvaluationRecord{
			Gate: e.Gate, Outcome: e.Verdict, Target: e.Target, Escalated: e.Escalated,
		}
		if e.Runner != nil {
			rec.RepassAttempt = annotationInt(e.Runner, "repassAttempt")
			rec.GateAttempt = annotationInt(e.Runner, "gateAttempt")
			rec.RepassTarget = annotationString(e.Runner, "repassTarget")
			rec.Reason = annotationString(e.Runner, "reason")
		}
		out = append(out, rec)
	}
	return out
}

func diffGateEvaluations(side string, got, want []gateEvaluationRecord) error {
	if len(got) != len(want) {
		return fmt.Errorf("%s evaluated %d gate(s), want %d:\n  got:  %s\n  want: %s",
			side, len(got), len(want), joinGateEvaluations(got), joinGateEvaluations(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("%s gate evaluation %d diverges:\n  got:  %s\n  want: %s",
				side, i+1, got[i], want[i])
		}
	}
	return nil
}

func joinGateEvaluations(records []gateEvaluationRecord) string {
	if len(records) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(records))
	for _, r := range records {
		parts = append(parts, "["+r.String()+"]")
	}
	return strings.Join(parts, " ")
}

// infraEscalation is the terminal both sides must reach on these fixtures: the
// escalate control branch, taken because the INFRASTRUCTURE budget ran out.
// Naming the reason code is what separates "escalated at the same time" from
// "escalated for the same reason" — a side that ran out of the policy budget
// instead lands on the same stage with a different explanation.
func requireInfraEscalation(side paritySide, gateName string, wantAttempt int) error {
	evaluations := gateEvaluations(side)
	if len(evaluations) == 0 {
		return fmt.Errorf("%s evaluated no gates at all", side.Name)
	}
	last := evaluations[len(evaluations)-1]
	if last.Gate != gateName || !last.Escalated || last.Outcome != gate.OutcomeInfra {
		return fmt.Errorf("%s final gate evaluation = %s, want an escalated infra outcome on %q",
			side.Name, last, gateName)
	}
	if last.RepassAttempt != wantAttempt {
		return fmt.Errorf("%s escalated on infrastructure repass %d, want %d (the budget is %d): %s",
			side.Name, last.RepassAttempt, wantAttempt, gate.DefaultMaxInfrastructureRepasses, last)
	}
	if last.Reason != gate.ReasonInfrastructureBudgetExhausted {
		return fmt.Errorf("%s escalation reason = %q, want %q — an exhausted infrastructure retry budget is not "+
			"policy repass churn, and telemetry keys on the difference",
			side.Name, last.Reason, gate.ReasonInfrastructureBudgetExhausted)
	}
	return nil
}

// --- the bound --------------------------------------------------------------

func premiseInfraRepassBudgetBound(obs parityObservation) error {
	// Anti-vacuity, in order of what could silently stop being true: the gate
	// must really answer INFRA (not fail), the branch must really RE-ENTER
	// local-ci (not route onward, which charges nothing), and the budget must
	// really be the infrastructure one.
	want := []gateEvaluationRecord{
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "local-ci",
			RepassAttempt: 1, GateAttempt: 1, RepassTarget: "local-ci"},
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "local-ci",
			RepassAttempt: 2, GateAttempt: 2, RepassTarget: "local-ci"},
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "park-escalated", Escalated: true,
			RepassAttempt: 3, GateAttempt: 3, RepassTarget: "local-ci",
			Reason: gate.ReasonInfrastructureBudgetExhausted},
	}
	if err := diffGateEvaluations("runner", gateEvaluations(obs.Runner), want); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — this row exists to pin the INFRASTRUCTURE bound (%d), so the runner must charge the "+
				"infrastructure counter and escalate one attempt past it",
			err, gate.DefaultMaxInfrastructureRepasses)
	}
	if err := requireStagesDispatched(obs.Runner,
		[]string{"implement", "local-ci", "local-ci", "local-ci", "park-escalated"}); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — two retries and no more is what a budget of %d means in dispatches",
			err, gate.DefaultMaxInfrastructureRepasses)
	}
	if err := requireInfraEscalation(obs.Runner, "local-gate", gate.DefaultMaxInfrastructureRepasses+1); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	return nil
}

func checkInfraRepassBudgetBound(obs parityObservation) error {
	if err := diffGateEvaluations("engine", gateEvaluations(obs.Engine), gateEvaluations(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — the engine bounded infrastructure repasses by the POLICY budget (runcontrol.MaxRepassesForGate, "+
				"default %d) instead of gate.DefaultMaxInfrastructureRepasses (%d), so an infrastructure-only run "+
				"got an extra retry there (#3930)",
			err, gate.DefaultMaxRepasses, gate.DefaultMaxInfrastructureRepasses)
	}
	if err := requireInfraEscalation(obs.Engine, "local-gate", gate.DefaultMaxInfrastructureRepasses+1); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}

// --- independence and the reset ---------------------------------------------

func premiseInfraRepassBudgetIndependence(obs parityObservation) error {
	want := []gateEvaluationRecord{
		// The first infrastructure sequence spends the whole budget...
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "local-ci",
			RepassAttempt: 1, GateAttempt: 1, RepassTarget: "local-ci"},
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "local-ci",
			RepassAttempt: 2, GateAttempt: 2, RepassTarget: "local-ci"},
		// ...the content failure opens a POLICY budget that those two retries
		// did not touch (repassAttempt 1, not 3), and resets the gate's
		// infrastructure evaluation count (gateAttempt 1, not 3)...
		{Gate: "local-gate", Outcome: gate.OutcomeFail, Target: "implement",
			RepassAttempt: 1, GateAttempt: 1, RepassTarget: "implement"},
		// ...and returns the infrastructure budget in full.
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "local-ci",
			RepassAttempt: 1, GateAttempt: 1, RepassTarget: "local-ci"},
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "local-ci",
			RepassAttempt: 2, GateAttempt: 2, RepassTarget: "local-ci"},
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "park-escalated", Escalated: true,
			RepassAttempt: 3, GateAttempt: 3, RepassTarget: "local-ci",
			Reason: gate.ReasonInfrastructureBudgetExhausted},
	}
	if err := diffGateEvaluations("runner", gateEvaluations(obs.Runner), want); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — the row's whole claim is that the two budgets are independent and that a non-infra outcome "+
				"returns the infrastructure retries the run spent, so the runner must exhibit both", err)
	}
	if err := requireInfraEscalation(obs.Runner, "local-gate", gate.DefaultMaxInfrastructureRepasses+1); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	return nil
}

func checkInfraRepassBudgetIndependence(obs parityObservation) error {
	if err := diffGateEvaluations("engine", gateEvaluations(obs.Engine), gateEvaluations(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — the engine charged infrastructure and content repasses to ONE counter and never reset it, so an "+
				"infrastructure retry from earlier in the run permanently consumed budget the runner had returned "+
				"(#3930)", err)
	}
	return checkAllSurfaces(obs)
}

// --- per-gate vs per-target -------------------------------------------------

func premiseInfraRepassCounterSeparation(obs parityObservation) error {
	want := []gateEvaluationRecord{
		// implement's budget, charged by local-gate...
		{Gate: "local-gate", Outcome: gate.OutcomeFail, Target: "implement",
			RepassAttempt: 1, GateAttempt: 1, RepassTarget: "implement"},
		// ...an infrastructure outcome, which counts in the OTHER per-gate
		// counter (gateAttempt 1, not 2) and charges the OTHER target...
		{Gate: "local-gate", Outcome: gate.OutcomeInfra, Target: "local-ci",
			RepassAttempt: 1, GateAttempt: 1, RepassTarget: "local-ci"},
		// ...a content failure again: the gate's policy count restarts at 1
		// because the class changed, while implement's cumulative budget
		// continues at 2. That pair of numbers on one event is the row.
		{Gate: "local-gate", Outcome: gate.OutcomeFail, Target: "implement",
			RepassAttempt: 2, GateAttempt: 1, RepassTarget: "implement"},
		{Gate: "local-gate", Outcome: gate.OutcomePass, Target: "verify"},
		// The SECOND gate charges the SAME target budget (3), with its own
		// per-gate counter starting at 1.
		{Gate: "verify-gate", Outcome: gate.OutcomeFail, Target: "implement",
			RepassAttempt: 3, GateAttempt: 1, RepassTarget: "implement"},
		// A PASSING gate re-entering a completed stage charges verify's own
		// budget — the budget is about re-entry, not about failure — while the
		// gate's per-gate counter is cleared by the pass. Two numbers moving in
		// opposite directions on one event, which is the separation this row is
		// named for.
		{Gate: "local-gate", Outcome: gate.OutcomePass, Target: "verify",
			RepassAttempt: 1, RepassTarget: "verify"},
		{Gate: "verify-gate", Outcome: gate.OutcomeFail, Target: "park-escalated", Escalated: true,
			RepassAttempt: 4, GateAttempt: 2, RepassTarget: "implement",
			Reason: gate.ReasonRepassBudgetExhausted},
	}
	if err := diffGateEvaluations("runner", gateEvaluations(obs.Runner), want); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — the runner must keep the per-GATE evaluation counters (which cross-reset by class) apart from "+
				"the per-TARGET budget (which is cumulative and shared between the two gates), or the row has "+
				"nothing to compare", err)
	}
	return nil
}

func checkInfraRepassCounterSeparation(obs parityObservation) error {
	if err := diffGateEvaluations("engine", gateEvaluations(obs.Engine), gateEvaluations(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — the engine advanced ONE per-gate counter for every non-pass regardless of class, so its "+
				"gateAttempt (the number gate.started reports and an interrupted-evaluator recovery reads) drifts "+
				"from the runner's the moment an infrastructure outcome interleaves with content failures (#3930)",
			err)
	}
	return checkAllSurfaces(obs)
}

// --- resumed / replayed histories -------------------------------------------

func premiseInfraRepassBudgetResumed(obs parityObservation) error {
	if err := requireInfraEscalation(obs.Runner, "local-gate", gate.DefaultMaxInfrastructureRepasses+1); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	rebuilt, err := rebuildRepassBudget(obs.Runner)
	if err != nil {
		return errParityPremisef(obs.Case.Row, "runner: %v", err)
	}
	// The reconstruction has to be non-trivial in BOTH classes, or the row
	// compares two empty budgets and reports agreement.
	if rebuilt.RepassAttempts["implement"] == 0 || rebuilt.InfrastructureRepassAttempts["local-ci"] == 0 {
		return errParityPremisef(obs.Case.Row,
			"runner journal rebuilds policy=%v infrastructure=%v — this row needs BOTH budgets to have been "+
				"charged, or it proves nothing about keeping them apart across a resume",
			rebuilt.RepassAttempts, rebuilt.InfrastructureRepassAttempts)
	}
	return nil
}

func checkInfraRepassBudgetResumed(obs parityObservation) error {
	runnerBudget, err := rebuildRepassBudget(obs.Runner)
	if err != nil {
		return errParityRow(obs.Case.Row, "runner: %v", err)
	}
	engineBudget, err := rebuildRepassBudget(obs.Engine)
	if err != nil {
		return errParityRow(obs.Case.Row, "engine: %v", err)
	}
	if err := diffRepassBudgets(runnerBudget, engineBudget); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — a resume (internal/runner/resume.go) and a replay both rebuild the budget from these journaled "+
				"numbers, so a side that journals a collapsed count hands its own recovery a budget that is "+
				"already spent (#3930)", err)
	}
	// Same numbers is necessary; the same DECISION is what the numbers are
	// for. Charge one more evaluation of each class through the shared helper
	// and require both reconstructions to route identically — this is the
	// resumed run's next step, computed from what each side actually wrote
	// down.
	g := obs.Case.Spec.Gates[0]
	for _, outcome := range []string{gate.OutcomeInfra, gate.OutcomeFail} {
		target := g.Branches[outcome]
		fromRunner := runnerBudget.Charge(g, outcome, target, true, gate.DefaultMaxRepasses)
		fromEngine := engineBudget.Charge(g, outcome, target, true, gate.DefaultMaxRepasses)
		if fromRunner != fromEngine {
			return errParityRow(obs.Case.Row,
				"a run resumed from each side's journal makes a different next decision on a %q outcome:\n"+
					"  from the runner's journal: %+v\n  from the engine's journal:  %+v",
				outcome, fromRunner, fromEngine)
		}
	}
	return checkAllSurfaces(obs)
}

// rebuildRepassBudget reconstructs the run's repass accounting from a side's
// journal alone, the way internal/runner/resume.go's seeds do: per class, keyed
// by the journaled repassTarget (which survives an escalation override, unlike
// Target), taking the highest attempt seen and applying the same cross-resets
// the live budget applies.
//
// It reads ONLY what a recovering process has — the events — and never the
// in-memory counters, which is the whole point: this is what a resumed runner
// or a replaying worker would start from.
func rebuildRepassBudget(side paritySide) (gate.RepassBudget, error) {
	budget := gate.RepassBudget{
		Attempts:                     map[string]int{},
		InfrastructureAttempts:       map[string]int{},
		RepassAttempts:               map[string]int{},
		InfrastructureRepassAttempts: map[string]int{},
	}
	infraTargets := map[string]string{}
	for _, rec := range gateEvaluations(side) {
		switch rec.Outcome {
		case gate.OutcomePass:
			budget.Attempts[rec.Gate] = 0
			budget.InfrastructureAttempts[rec.Gate] = 0
		case gate.OutcomeInfra:
			budget.Attempts[rec.Gate] = 0
			budget.InfrastructureAttempts[rec.Gate] = rec.GateAttempt
		default:
			budget.InfrastructureAttempts[rec.Gate] = 0
			budget.Attempts[rec.Gate] = rec.GateAttempt
		}
		if rec.Outcome != gate.OutcomeInfra {
			if target := infraTargets[rec.Gate]; target != "" {
				budget.InfrastructureRepassAttempts[target] = 0
			}
		}
		if rec.RepassTarget == "" {
			if rec.RepassAttempt != 0 {
				return budget, fmt.Errorf("gate.evaluated for %q journaled repassAttempt %d with no repassTarget: "+
					"the counter cannot be re-keyed on resume", rec.Gate, rec.RepassAttempt)
			}
			continue
		}
		if rec.Outcome == gate.OutcomeInfra {
			infraTargets[rec.Gate] = rec.RepassTarget
			budget.InfrastructureRepassAttempts[rec.RepassTarget] = rec.RepassAttempt
			continue
		}
		budget.RepassAttempts[rec.RepassTarget] = rec.RepassAttempt
	}
	return budget, nil
}

func diffRepassBudgets(runnerBudget, engineBudget gate.RepassBudget) error {
	for _, counter := range []struct {
		name          string
		runner, engin map[string]int
	}{
		{"per-gate policy attempts", runnerBudget.Attempts, engineBudget.Attempts},
		{"per-gate infrastructure attempts", runnerBudget.InfrastructureAttempts, engineBudget.InfrastructureAttempts},
		{"per-target policy budget", runnerBudget.RepassAttempts, engineBudget.RepassAttempts},
		{"per-target infrastructure budget", runnerBudget.InfrastructureRepassAttempts, engineBudget.InfrastructureRepassAttempts},
	} {
		if err := diffCounterMaps(counter.name, counter.runner, counter.engin); err != nil {
			return err
		}
	}
	return nil
}

// diffCounterMaps compares two counter maps by KEY, in sorted order, so the
// failure message is stable and a map with an explicit zero reads the same as
// one without the key — which is exactly the backwards-compatibility contract
// for a history recorded before a counter existed.
func diffCounterMaps(name string, runnerCounts, engineCounts map[string]int) error {
	keys := map[string]bool{}
	for key := range runnerCounts {
		keys[key] = true
	}
	for key := range engineCounts {
		keys[key] = true
	}
	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)
	for _, key := range names {
		if runnerCounts[key] != engineCounts[key] {
			return fmt.Errorf("%s for %q rebuilds as runner=%d engine=%d",
				name, key, runnerCounts[key], engineCounts[key])
		}
	}
	return nil
}
