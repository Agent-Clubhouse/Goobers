package engine

// Parity rows for plan items E4 and E5 (#3882): the three implementation-lane
// decisions that determine whether an agentic reviewer is dispatched AT ALL.
//
// Every row here is a NEGATIVE row in the sense that matters. The behaviour
// being ported is not "produce this verdict" — both lanes produced acceptable
// verdicts before the port — it is "do not ask a reviewer a question whose
// answer is already determined". A port that dispatches the reviewer anyway
// still reaches the same outcome; it just spends a model call to get there,
// and on a non-convergent repass loop it spends the whole budget. So these
// rows assert on the DISPATCH, not the decision.
//
// The mechanism that makes that assertion sharp is the harness's scripted
// reviewer: a gate the row does not script a verdict for FAILS LOUDLY when it
// is dispatched, on either lane. There is no "quietly returns a zero verdict"
// path for an over-eager port to hide in.
//
// # The diff seam, and why the engine's is scripted while the runner's is not
//
// The local runner reads a real git worktree, so its diff is whatever the
// fixture's stages actually left behind — and scriptedExec, which drives both
// lanes, writes no files. The runner therefore observes an EMPTY diff. The
// engine's fake provisioner is told to report the same empty diff
// (EngineWorkspaceDiffs), which is what makes these rows a comparison of two
// DECISIONS over one observation rather than of one real observation against
// one fictional one. Scripting a non-empty diff on the engine side would prove
// nothing about the runner, so no row here does.
//
// That bounds what this file can cover, and the bound is stated rather than
// hidden: a genuinely non-empty diff on both lanes would need the scripted
// stages to write into a real worktree on one side and a fake on the other.
// The #316 duplicate-diff dedup and the #3384 diff artifact/pointer are
// therefore pinned by internal/engine's own deterministic tests
// (implementationlane_test.go) and by internal/gate's shared-helper tests,
// which is where the two lanes' single implementation of each already lives.

import (
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
)

// reviewerGate is an AGENTIC gate — the only evaluator any of this applies to.
func reviewerGate(name string, branches map[string]string) apiv1.Gate {
	return apiv1.Gate{
		Name:      name,
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
		Branches:  branches,
	}
}

// cachedVerdictOutputs is a deterministic subject stage's outputs carrying a
// digest-matched prior verdict, in the shape merge-review's
// gather-sibling-context actually produces (#523): the verdict as a JSON
// STRING under cachedVerdictJson, because a stage's Outputs are scalar-only.
func cachedVerdictOutputs(decision apiv1.VerdictDecision, rationale string) map[string]interface{} {
	raw, err := json.Marshal(apiv1.Verdict{Decision: decision, Rationale: rationale})
	if err != nil {
		panic(fmt.Sprintf("encode cached verdict fixture: %v", err))
	}
	return map[string]interface{}{runner.CachedVerdictOutputKey: string(raw)}
}

func init() {
	// --- E4: the cached verdict short-circuit -------------------------------
	//
	// The subject is DETERMINISTIC because that is what the behaviour is for:
	// merge-review's gather-sibling-context re-runs the very check the
	// reviewer would run, finds a digest-matched prior verdict, and hands it
	// forward. A deterministic subject also keeps the #415 empty-diff
	// fast-fail out of the way, so what this row grades is the cache and only
	// the cache.
	registerParityRow(parityCase{
		Row:        rowCachedVerdictShortCircuit,
		Name:       "a subject carrying a digest-matched verdict routes its gate with no reviewer on either lane",
		DSLVersion: "2.0",
		UsesRepo:   true,
		Spec: fixtureSpec("implement",
			[]apiv1.Task{detTask("implement", "review")},
			[]apiv1.Gate{reviewerGate("review", map[string]string{
				"pass":          wf.TerminalComplete,
				"fail":          wf.TargetAbort,
				"needs-changes": "implement",
			})},
		),
		Script: map[string][]scriptedCall{
			"implement": {succeed(cachedVerdictOutputs(apiv1.VerdictPass, "the sibling run already reviewed this identical tree"))},
		},
		// Deliberately no Verdicts entry for "review": a dispatched reviewer
		// is a failed run here, on whichever lane dispatches it.
		Premise: premiseCachedVerdictShortCircuit,
		Check:   checkCachedVerdictShortCircuit,
	})

	// --- E5: the empty-diff fast-fail ---------------------------------------
	registerParityRow(parityCase{
		Row:        rowEmptyDiffFastFail,
		Name:       "an agentic stage that changed nothing fails its gate with no reviewer on either lane",
		DSLVersion: "2.0",
		UsesRepo:   true,
		Spec: fixtureSpec("implement",
			[]apiv1.Task{agenticTask("implement", "review")},
			[]apiv1.Gate{reviewerGate("review", map[string]string{
				"pass":          wf.TerminalComplete,
				"fail":          wf.TargetAbort,
				"needs-changes": "implement",
			})},
		),
		Script: map[string][]scriptedCall{
			// A stage that reports success while changing nothing. This is the
			// case the fast-fail exists for: the reviewer cannot pass an empty
			// patch, so asking it is pure cost.
			"implement": {succeed(map[string]interface{}{"claimed": "done"})},
		},
		// The engine's workspace observes the same empty diff the runner's
		// real worktree does.
		EngineWorkspaceDiffs: map[string][]byte{"review": nil},
		Premise:              premiseEmptyDiffFastFail,
		Check:                checkEmptyDiffFastFail,
	})

	// --- E5's scope, the negative that forbids a blanket fast-fail ----------
	//
	// The identical empty diff over a DETERMINISTIC subject must still be
	// reviewed: a verification or publication stage legitimately changes
	// nothing, and fast-failing it would break every such lane. Without this
	// row, "fast-fail whenever the diff is empty" would leave the row above
	// green.
	registerParityRow(parityCase{
		Row:        rowEmptyDiffDeterministicSubject,
		Name:       "the same empty diff over a deterministic subject still dispatches the reviewer on both lanes",
		DSLVersion: "2.0",
		UsesRepo:   true,
		Spec: fixtureSpec("verify",
			[]apiv1.Task{detTask("verify", "review")},
			[]apiv1.Gate{reviewerGate("review", map[string]string{
				"pass":          wf.TerminalComplete,
				"fail":          wf.TargetAbort,
				"needs-changes": "verify",
			})},
		),
		Script: map[string][]scriptedCall{
			"verify": {succeed(map[string]interface{}{"checks": "green"})},
		},
		Verdicts: map[string][]apiv1.Verdict{
			"review": {{Decision: apiv1.VerdictPass, Summary: "verification output is correct"}},
		},
		EngineWorkspaceDiffs: map[string][]byte{"review": nil},
		Premise:              premiseEmptyDiffDeterministicSubject,
		Check:                checkEmptyDiffDeterministicSubject,
	})
}

// --- premises ---------------------------------------------------------------

// premiseCachedVerdictShortCircuit: the RUNNER really does short-circuit. If
// it ever stopped, this row would be comparing two lanes that both dispatch a
// reviewer and would say nothing about the cache.
func premiseCachedVerdictShortCircuit(obs parityObservation) error {
	if n := countReviewerDispatches(obs.Runner, "review"); n != 0 {
		return errParityPremisef(obs.Case.Row,
			"the runner dispatched the reviewer %d time(s) for a subject carrying a cached verdict; "+
				"this row exists to compare a short-circuit the runner is supposed to take", n)
	}
	if obs.Runner.Terminal.Status != string(StatusCompleted) {
		return errParityPremisef(obs.Case.Row,
			"the runner ended %q, want completed — the cached PASS is supposed to route the gate",
			obs.Runner.Terminal.Status)
	}
	return nil
}

// premiseEmptyDiffFastFail: the runner really fast-fails, and really does so
// WITHOUT a reviewer.
func premiseEmptyDiffFastFail(obs parityObservation) error {
	if n := countReviewerDispatches(obs.Runner, "review"); n != 0 {
		return errParityPremisef(obs.Case.Row,
			"the runner dispatched the reviewer %d time(s) for an empty diff; the fast-fail is supposed to "+
				"resolve the gate without one", n)
	}
	if err := requireGateReason(obs.Runner, "review", gate.ReasonRepassBudgetExhausted); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	return nil
}

// premiseEmptyDiffDeterministicSubject: the runner really DOES review here.
// This is the premise that makes the row a scope assertion rather than a
// second copy of the row above.
func premiseEmptyDiffDeterministicSubject(obs parityObservation) error {
	if n := countReviewerDispatches(obs.Runner, "review"); n != 1 {
		return errParityPremisef(obs.Case.Row,
			"the runner dispatched the reviewer %d time(s), want 1 — a deterministic subject's empty diff "+
				"is not a fast-fail, and if the runner has started treating it as one this row is comparing "+
				"the wrong thing", n)
	}
	return nil
}

// --- checks -----------------------------------------------------------------

func checkCachedVerdictShortCircuit(obs parityObservation) error {
	if err := requireSameReviewerDispatches(obs, "review"); err != nil {
		return err
	}
	if err := requireCacheHitRecorded(obs.Engine, "review"); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}

func checkEmptyDiffFastFail(obs parityObservation) error {
	if err := requireSameReviewerDispatches(obs, "review"); err != nil {
		return err
	}
	if err := requireGateReason(obs.Engine, "review", gate.ReasonRepassBudgetExhausted); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}

func checkEmptyDiffDeterministicSubject(obs parityObservation) error {
	if err := requireSameReviewerDispatches(obs, "review"); err != nil {
		return err
	}
	return checkAllSurfaces(obs)
}

// --- shared assertions ------------------------------------------------------

// requireSameReviewerDispatches is the assertion these rows exist for: the two
// lanes must ask the reviewer the same number of questions. Counted from the
// recorded ENVELOPES, which is the only place a dispatch that produced no
// journal entry is visible at all.
func requireSameReviewerDispatches(obs parityObservation, gateName string) error {
	got, want := countReviewerDispatches(obs.Engine, gateName), countReviewerDispatches(obs.Runner, gateName)
	if got != want {
		return errParityRow(obs.Case.Row,
			"engine dispatched the %q reviewer %d time(s), runner %d — an extra reviewer invocation is the "+
				"whole cost this behaviour exists to avoid, and a missing one changes what the gate decided on",
			gateName, got, want)
	}
	return nil
}

// countReviewerDispatches counts recorded envelopes for the gate's own stage.
// A gate's envelope is distinguishable from a task's by its stage name, which
// the walk composes from the gate name on both lanes.
func countReviewerDispatches(side paritySide, gateName string) int {
	n := 0
	for _, env := range side.Envelopes {
		if env.Stage == gateName {
			n++
		}
	}
	return n
}

// requireGateReason checks the journaled REASON on the gate's own EVALUATION.
//
// Scoped to gate.evaluated deliberately: a runner.annotation also carries a
// gate name and a Runner map, so a looser scan would match one of those and
// "find" an empty reason on a lane that journals none at all, which is a
// vacuously passing assertion rather than a check.
func requireGateReason(side paritySide, gateName, want string) error {
	seen := []string{}
	for _, e := range side.Events {
		if e.Type != journal.EventGateEvaluated || e.Gate != gateName {
			continue
		}
		reason, _ := e.Runner["reason"].(string)
		if reason == want {
			return nil
		}
		seen = append(seen, reason)
	}
	return fmt.Errorf("%s journaled gate.evaluated reasons %q for gate %q, want one carrying %q",
		side.Name, seen, gateName, want)
}

// requireCacheHitRecorded checks the cache hit is journaled, not merely acted
// on: an operator reading the run has to be able to see why no reviewer ran.
func requireCacheHitRecorded(side paritySide, gateName string) error {
	for _, e := range side.Events {
		if e.Type != journal.EventGateEvaluated || e.Gate != gateName {
			continue
		}
		if hit, _ := e.Runner["verdictCacheHit"].(bool); hit {
			return nil
		}
	}
	return fmt.Errorf("%s journaled no verdictCacheHit for gate %q", side.Name, gateName)
}
