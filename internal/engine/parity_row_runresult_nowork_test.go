package engine

// Parity row E2-runresult-nowork — CLOSED by plan item E2, must stay GREEN.
//
// Inventory row: "NoWork short-circuit accounting: Result.NoWork = terminal
// no-work at step 1 (#233), consumed by the scheduler's schedule idle backoff
// (recordScheduledPollResult)." Runner site:
// internal/runner/run.go:3606 (res.NoWork = steps == 1);
// internal/localscheduler/scheduler.go:2304. Engine: RunResult.NoWork
// (engine.go) set in taskOutcome's ResultNoWork arm when steps == 1, so an
// engine-driven backlog-curation run that finds nothing to claim is
// distinguishable from one that did real work and the scheduler backs off its
// idle polling (nowork_backoff_test.go pins that far side end to end).
//
// Invisible to the journal surface: both sides journal
// run.finished(status=completed) identically. It is only visible in the value
// the daemon's Starter maps into StartResult, which is what parityTerminal
// compares.
//
// The fixture is the real backlog-curation first stage (reconcile-backlog)
// reporting no-work, which is the production shape: a curation tick whose very
// first stage finds nothing.
//
// The harness reads the field by name off the marshalled result
// (engineRunResultNoWork) rather than referencing res.NoWork, which is what let
// this row be written failing-first; it is left that way deliberately, so the
// row keeps pinning the WIRE shape the plan specifies — a bool under the
// "noWork" key, omitted when false — and not merely a Go field name.

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func init() {
	registerParityRow(parityCase{
		Row:     rowRunResultNoWork,
		Name:    "no-work at step 1 sets the terminal NoWork accounting",
		Lane:    "backlog-curation.yaml",
		Build:   buildRunResultNoWorkCase,
		Premise: premiseRunResultNoWork,
	})
}

func buildRunResultNoWorkCase(t *testing.T, c *parityCase) {
	t.Helper()
	lane := backlogCurationLane(t)
	c.Spec = laneChain(t, lane, "reconcile-backlog", "implementation-feedback")
	c.DSLVersion = lane.DSLVersion
	c.UsesRepo = true
	c.Script = map[string][]scriptedCall{
		"reconcile-backlog": {{result: apiv1.ResultEnvelope{Status: apiv1.ResultNoWork, Summary: "empty tick"}}},
	}
}

// premiseRunResultNoWork pins the runner's own accounting, so the row cannot
// pass by both sides reporting false.
//
// It is the row's PREMISE, not the head of its Check, and that distinction is
// the whole reason this row is credible: while the row was on
// parityExpectedFailures a graded assertion here would have been swallowed into
// the "expected failure, still open" arm — deleting `res.NoWork = steps == 1`
// from the local runner left this suite green, printing this very sentence as
// it went. The row is closed now, but the premise is what keeps "both sides
// report false" from ever reading as agreement.
func premiseRunResultNoWork(obs parityObservation) error {
	if !obs.Runner.Terminal.NoWork {
		return errParityPremisef(obs.Case.Row,
			"runner did not report NoWork for a step-1 no-work terminal (%s) — the fixture no longer exercises #233",
			obs.Runner.Terminal)
	}
	return nil
}

func init() {
	// The negative half of the same inventory row: a no-work at step 3 must
	// NOT set NoWork. It is registered as its own row id because
	// parityExpectedFailures is keyed by row, and this half already AGREES
	// (both sides report false) — keeping it separate means the port that
	// closes the positive half cannot regress the negative one unnoticed.
	registerParityRow(parityCase{
		Row:     rowRunResultNoWorkLateStage,
		Name:    "no-work after step 1 leaves the NoWork accounting clear",
		Lane:    "backlog-curation.yaml",
		Build:   buildRunResultNoWorkLateCase,
		Premise: premiseRunResultNoWorkLate,
	})
}

func buildRunResultNoWorkLateCase(t *testing.T, c *parityCase) {
	t.Helper()
	lane := backlogCurationLane(t)
	c.Spec = laneChain(t, lane, "reconcile-backlog", "implementation-feedback", "sample-ready-pool")
	c.DSLVersion = lane.DSLVersion
	c.UsesRepo = true
	c.Script = map[string][]scriptedCall{
		"reconcile-backlog":       {succeed(map[string]interface{}{"backlog-reconciliation": "0"})},
		"implementation-feedback": {succeed(map[string]interface{}{"implementation-feedback": "0"})},
		"sample-ready-pool":       {{result: apiv1.ResultEnvelope{Status: apiv1.ResultNoWork, Summary: "nothing left"}}},
	}
}

// premiseRunResultNoWorkLate is the negative half's anti-vacuity guard: this row
// is GREEN because both sides report false, so "the runner really reached a
// late-stage no-work and reported false" is precisely what has to hold for the
// green to mean anything.
func premiseRunResultNoWorkLate(obs parityObservation) error {
	if obs.Runner.Terminal.NoWork {
		return errParityPremisef(obs.Case.Row,
			"runner reported NoWork for a no-work at step 3 (%s) — #233 scopes it to step 1", obs.Runner.Terminal)
	}
	// The stage that reports no-work must actually be reached: a walk that
	// stopped earlier would report false for the wrong reason and this row
	// would be green on a fixture that never tested the boundary.
	if err := requireStagesDispatched(obs.Runner, []string{"reconcile-backlog", "implementation-feedback", "sample-ready-pool"}); err != nil {
		return errParityPremisef(obs.Case.Row, "%v — the late no-work boundary is never reached", err)
	}
	return nil
}
