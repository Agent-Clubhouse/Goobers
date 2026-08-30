package engine

// Parity rows E2-nonretryable-escalation, E2-nonretryable-escalation-default,
// E2-retryable-failure-still-gated and E2-unrecognized-failure-still-gated —
// CLOSED by plan item E2, must stay GREEN.
//
// Inventory row: "#415 non-retryable escalation bypass: a failure with
// error.retryable=false and a recognized escalate code (ISSUE_OVER_SCOPE /
// NEEDS_DECOMPOSITION / ISSUE_NOT_APPLICABLE) routes to the stage's escalate
// control branch, bypassing the Next gate's evaluator and its repass loop."
// Runner site: internal/runner/run.go's stepTask arm plus isNonRetryableEscalation
// / taskEscalationTarget (run.go:5082-5120). Engine: taskOutcome's ResultFailure
// arm now consults runner.IsNonRetryableEscalation before the "Next is a gate"
// branch and routes through escalationOutcome (retrydecision.go).
//
// Why four rows, and why the last two are the load-bearing ones. The bypass is
// a SHORTCUT: it makes a failure skip the machinery every other failure goes
// through. A port that simply escalated every failure would turn both positive
// rows green and quietly delete the review→implement repass loop from the
// engine — which is the entire implementation lane. So two negative rows walk
// the failures that must STILL be gated:
//
//   - retryable=true carrying the very same escalate code, and
//   - retryable=false carrying a code the escalate set does not name,
//
// and assert both sides evaluated the gate and took its FAIL branch. Because
// the fixture's fail and escalate branches are distinct stages, "bypassed" and
// "gated" are directly observable as park-escalated vs park-failed rather than
// inferred. Each row's premise is the runner half of that, ungraded.
//
// The terminal each row compares is the point the critic's correction to
// finding 002 makes: an @abort escalate branch ends journal.PhaseAborted
// (StatusBlocked) and a terminal-complete one ends journal.PhaseEscalated, NOT
// PhaseCompleted — a run that escalated an un-scopeable item was never done.
// parityTerminal compares status and finalState on both sides, so a hook frame
// built on the journal phase (engine.PhaseForStatus) sees the same terminal the
// local runner produces.
//
// The #3363 disposition NOTIFICATION the runner posts alongside the bypass is
// out of scope for these rows by construction: it is an outbound side effect
// with no engine seam, it is fired through runner.Config.Notify (unset here on
// both sides), and it changes no transition. What the inventory row is about —
// where the walk goes — is what is compared.

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// escalateFailure scripts the stage self-identifying a non-retryable business
// disposition: status failure, error.retryable false, and one of the runner's
// recognized escalate codes.
func escalateFailure(code string) scriptedCall {
	return scriptedCall{result: apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: "the issue as filed spans four subsystems",
		Error:   &apiv1.ErrorInfo{Code: code, Message: code, Retryable: false},
	}}
}

// parityEscalationSpec is implement -> review, where review's branches carry
// both the control branch under test (escalate) and an ordinary fail branch.
// The two targets are DISTINCT stages, which is what makes the bypass and its
// absence directly observable: a walk that bypassed the gate lands on
// park-escalated, one that evaluated it lands on park-failed.
//
// Neither branch re-enters implement. That is deliberate: a fail branch that
// loops back is a RETRY, and the local runner injects a learning-episode
// artifact on every retry (recordLearningInjection) which the engine has no
// counterpart for — a divergence that is real, is not E2's, and would drown
// these rows in an unrelated conformance diff. The repass loop is exercised by
// rowRetryDecisionAnnotation, which is about the retry path and carries the
// bounded carve-out for it.
func parityEscalationSpec(escalate string) apiv1.WorkflowSpec {
	branches := map[string]string{"pass": wf.TerminalComplete, "fail": "park-failed"}
	tasks := []apiv1.Task{
		detTask("implement", "review"),
		detTask("park-failed", wf.TargetAbort),
	}
	if escalate != "" {
		branches[wf.BranchEscalate] = escalate
		tasks = append(tasks, detTask(escalate, wf.TargetEscalate))
	}
	return fixtureSpec("implement", tasks, []apiv1.Gate{statusGate("review", branches)})
}

func init() {
	// The production shape finding 002's far-side evidence names: a fixture
	// whose implement stage returns ISSUE_OVER_SCOPE routes to a park stage.
	registerParityRow(parityCase{
		Row:        rowNonRetryableEscalation,
		Name:       "a non-retryable escalate code routes to the gate's escalate control branch",
		DSLVersion: "2.0",
		Spec:       parityEscalationSpec("park-escalated"),
		Script: map[string][]scriptedCall{
			"implement":      {escalateFailure("ISSUE_OVER_SCOPE")},
			"park-escalated": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseNonRetryableEscalation,
		Check:   checkNonRetryableEscalation,
	})

	// The default arm: no escalate control branch, so the run ends at
	// @escalate instead of entering the repass loop.
	registerParityRow(parityCase{
		Row:        rowNonRetryableEscalationDefault,
		Name:       "a non-retryable escalate code with no control branch ends the run escalated",
		DSLVersion: "2.0",
		Spec:       parityEscalationSpec(""),
		Script: map[string][]scriptedCall{
			"implement": {escalateFailure("NEEDS_DECOMPOSITION")},
		},
		Premise: premiseNonRetryableEscalationDefault,
		Check:   checkNonRetryableEscalationDefault,
	})

	// The negative half: a RETRYABLE failure carrying the very same escalate
	// code must still route into the gate, which sends it down the FAIL
	// branch — #415 is about a disposition the stage says re-running cannot
	// change, not about the code alone.
	registerParityRow(parityCase{
		Row:        rowRetryableFailureStillGated,
		Name:       "a retryable or unrecognized failure still routes through the Next gate",
		DSLVersion: "2.0",
		Spec:       parityEscalationSpec("park-escalated"),
		Script: map[string][]scriptedCall{
			"implement": {{result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "ISSUE_OVER_SCOPE", Message: "transient", Retryable: true},
			}}},
			"park-failed": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseRetryableFailureStillGated,
		Check:   checkRetryableFailureStillGated,
	})

	// ...and neither may a non-retryable failure carrying a code the escalate
	// set does not name. This is the arm that catches a port keyed on
	// error.retryable alone.
	//
	// The code is deliberately NOT "nonzero_exit": that one is recognized by
	// runner.RetryFailureClass, which puts the walk on the retry path and so
	// on the learning-episode injection the engine has no counterpart for
	// (see rowRetryDecisionAnnotation). Keeping this row off that path keeps
	// it a full checkAllSurfaces comparison with no carve-outs.
	registerParityRow(parityCase{
		Row:        rowUnrecognizedFailureStillGated,
		Name:       "a non-retryable failure with an unrecognized code still routes through the Next gate",
		DSLVersion: "2.0",
		Spec:       parityEscalationSpec("park-escalated"),
		Script: map[string][]scriptedCall{
			"implement": {{result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "assertion_failed", Message: "tests failed", Retryable: false},
			}}},
			"park-failed": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseRetryableFailureStillGated,
		Check:   checkRetryableFailureStillGated,
	})
}

// premiseNonRetryableEscalation is the anti-vacuity half: the RUNNER must
// really bypass the gate and dispatch the control-branch stage. Without this a
// fixture that never reached the disposition at all would leave the row green.
func premiseNonRetryableEscalation(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Runner, []string{"implement", "park-escalated"}); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — #415 must bypass the review gate and route to the escalate control branch exactly once", err)
	}
	if err := requireNoGateEvaluated(obs.Runner, "review"); err != nil {
		return errParityPremisef(obs.Case.Row, "%v — the bypass is what #415 IS", err)
	}
	return nil
}

// checkNonRetryableEscalation grades the engine against that, then the shared
// surfaces. The explicit dispatch assertion is here rather than left to the
// envelope diff because "the engine also dispatched park-escalated" is the
// row's subject, and a bare envelope diff would report it as an index mismatch.
func checkNonRetryableEscalation(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Engine, []string{"implement", "park-escalated"}); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	if err := requireNoGateEvaluated(obs.Engine, "review"); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}

// premiseNonRetryableEscalationDefault pins the runner's default arm: one
// dispatch, no gate, and an ESCALATED terminal — not a completed one.
func premiseNonRetryableEscalationDefault(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Runner, []string{"implement"}); err != nil {
		return errParityPremisef(obs.Case.Row, "%v — the bypass must terminate after one attempt", err)
	}
	if obs.Runner.Terminal.Status != StatusEscalated {
		return errParityPremisef(obs.Case.Row,
			"runner terminal was %s, want status %s — a bypassed disposition escalates, it does not complete",
			obs.Runner.Terminal, StatusEscalated)
	}
	return nil
}

func checkNonRetryableEscalationDefault(obs parityObservation) error {
	if obs.Engine.Terminal.Status != StatusEscalated {
		return errParityRow(obs.Case.Row, "engine terminal was %s, want status %s", obs.Engine.Terminal, StatusEscalated)
	}
	return checkAllSurfaces(obs)
}

// premiseRetryableFailureStillGated is the assertion that forbids "escalate
// every failure": the runner must EVALUATE the gate and take its fail branch,
// landing on park-failed rather than park-escalated.
func premiseRetryableFailureStillGated(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Runner, []string{"implement", "park-failed"}); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — this failure must still route through the Next gate's FAIL branch, not the escalate bypass", err)
	}
	if n := countGateEvaluations(obs.Runner, "review"); n != 1 {
		return errParityPremisef(obs.Case.Row,
			"runner evaluated gate %q %d time(s), want 1 — the gate is what must NOT be bypassed here", "review", n)
	}
	return nil
}

// checkRetryableFailureStillGated grades the engine against that on every
// surface. The dispatch assertion is spelled out rather than left to the
// envelope diff because "park-failed, not park-escalated" is the row's whole
// subject, and a bare envelope diff would report it as a stage-name mismatch
// with no hint that a bypass is what caused it.
func checkRetryableFailureStillGated(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Engine, []string{"implement", "park-failed"}); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — the engine must not bypass the gate for a retryable or unrecognized failure", err)
	}
	if n := countGateEvaluations(obs.Engine, "review"); n != 1 {
		return errParityRow(obs.Case.Row, "engine evaluated gate %q %d time(s), want 1", "review", n)
	}
	return checkAllSurfaces(obs)
}

// countGateEvaluations counts a side's gate.evaluated events for one gate.
func countGateEvaluations(side paritySide, gate string) int {
	n := 0
	for _, e := range side.Events {
		if e.Type == journal.EventGateEvaluated && e.Gate == gate {
			n++
		}
	}
	return n
}

// requireNoGateEvaluated asserts a gate was never evaluated — the observable
// half of "the evaluator was bypassed", read off the journal because an
// automated gate dispatches no invocation envelope.
func requireNoGateEvaluated(side paritySide, gate string) error {
	if n := countGateEvaluations(side, gate); n != 0 {
		return fmt.Errorf("%s evaluated gate %q %d time(s), want 0 — #415 bypasses the Next gate entirely",
			side.Name, gate, n)
	}
	return nil
}
