package engine

// Parity row E10-learning-episode-infra-forward-branch — plan item E10, the
// #3929 ruling, on the arm that actually carries it in production.
//
// Inventory row (finding 002), narrowing E10's producer row: "an infrastructure
// gate outcome whose branch routes ONWARD to a disposition stage is not a
// repass, and injects no learning episode on either runner."
//
// # Why a second forward-branch row
//
// E10-learning-episode-forward-branch walks a status-equals gate over a
// nonzero_exit failure. That reaches retryFailureClassForGateResult through its
// FIRST arm — retryFailureClass recognizes the error code and returns
// journal.AttemptPolicy. It is a synthetic shape: no shipped workflow has a
// status-equals gate whose fail branch routes onward to a real stage (the two
// that exist, docs-updater's docs-valid and tutor's three, route to @abort,
// which routeRetryDecision declines before the class is even consulted).
//
// The branch the ruling actually CHANGES is reached through the SECOND arm:
//
//	func retryFailureClassForGateResult(g, result, outcome) {
//	    class, knownOutcome, retryable := retryFailureClass(g, result)
//	    if !retryable && outcome == gate.OutcomeInfra {
//	        return journal.AttemptInfra, "", true      // <- this one
//	    }
//	    ...
//	}
//
// No other row in this table produces gate.OutcomeInfra, because every row that
// reaches a retry arm uses a status-equals gate and only the "failure-class"
// check can answer infra. So without this row the entire AttemptInfra path
// through the retry arm — the path production takes — is untested on both
// sides, and the ruling would be pinned only on a shape no lane has.
//
// # The production shape it reproduces
//
// reference-workflows/gaggles/goobers/workflows/pr-remediation.yaml:917-926
//
//	- name: local-gate
//	  evaluator: automated
//	  automated:
//	    check: failure-class
//	  branches:
//	    pass: guard-before-push
//	    fail: guard-before-implement
//	    infra: park-infrastructure-failure     # forward: never run
//	    escalate: park-escalated
//
// park-infrastructure-failure (:711-736) is a deterministic parking stage whose
// own escalation text is "local CI could not complete because of a retryable
// infrastructure failure; no implementation defect was established", and whose
// next is release-escalated-claim -> @escalate. It never re-attempts anything.
//
// Before #3929 the local runner committed a learning episode into it: a durable
// learning/episode-local-gate-<seq>.json artifact carrying a classification, a
// recommendedAction, correctionFeedback and a content-addressed signature that
// internal/gate.readEpisodeHistory correlates ACROSS RUNS — i.e. a fabricated
// defect signature for a pure infrastructure timeout, addressed to a stage
// whose contract is that no defect was established. The engine injected
// nothing. This row is that case, and it is now green from both directions.
//
// The fixture keeps the production names and the production branch set rather
// than minimal ones, because the row's argument is about what those stages ARE.
// A row over "gate/target" would prove the mechanism and lose the point.

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

func init() {
	registerParityRow(parityCase{
		Row:        rowLearningEpisodeInfraForwardBranch,
		Name:       "an infra outcome routing onward to a parking stage injects on neither runner",
		Lane:       "pr-remediation.yaml",
		DSLVersion: "2.0",
		Spec: fixtureSpec("implement",
			[]apiv1.Task{
				detTask("implement", "local-ci"),
				detTask("local-ci", "local-gate"),
				// The disposition stage. Its counterpart in the lane ends the
				// run at @escalate via release-escalated-claim; the row keeps
				// the terminal and drops the intermediate hop.
				detTask("park-infrastructure-failure", wf.TargetEscalate),
			},
			[]apiv1.Gate{failureClassGate("local-gate", map[string]string{
				"pass":  wf.TerminalComplete,
				"fail":  "implement",
				"infra": "park-infrastructure-failure",
			})},
		),
		Script: map[string][]scriptedCall{
			"implement": {succeed(map[string]interface{}{"committed": "true"})},
			// Retryable is the producer's own claim that the machinery failed
			// rather than the work — exactly what the lane's local-ci reports
			// on a runner/timeout fault, and the only input "failure-class"
			// reads to answer infra.
			"local-ci":                    {failRetryable("worktree_provision_failed", "runner host contention")},
			"park-infrastructure-failure": {succeed(map[string]interface{}{"escalationOutcome": "infrastructure-failure"})},
		},
		Premise: premiseLearningEpisodeInfraForwardBranch,
		Check:   checkLearningEpisodeInfraForwardBranch,
	})
}

// premiseLearningEpisodeInfraForwardBranch pins every link in the chain this
// row depends on, because each one is a way the fixture could quietly stop
// exercising the AttemptInfra arm while still reporting zero episodes:
//
//  1. the gate really resolved INFRA — asserted through the retry decision's
//     class, which is journal.AttemptInfra only on that arm;
//  2. the retry arm was really ENTERED — the annotation exists at all, which
//     means retryFailureClassForGateResult returned retryable;
//  3. the target really is FORWARD — repassAttempt 0, and the stage is
//     dispatched for the first time after the gate;
//  4. and the runner injects nothing into it.
func premiseLearningEpisodeInfraForwardBranch(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Runner,
		[]string{"implement", "local-ci", "park-infrastructure-failure"}); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — the infra branch must route ONWARD to a stage that has not run", err)
	}
	want := []retryDecisionRecord{{
		Stage: "local-ci", Gate: "local-gate", FailureClass: string(journal.AttemptInfra),
		FailureCode: "worktree_provision_failed", RepassAttempt: 0, Target: "park-infrastructure-failure",
	}}
	if err := diffRetryDecisions("runner", retryDecisions(obs.Runner), want); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — this row exists to cover retryFailureClassForGateResult's INFRA arm, which no other row "+
				"in this table reaches; a class of %q would mean the fixture fell back to the policy arm and "+
				"the infra path is untested again", err, journal.AttemptInfra)
	}
	if got := learningEpisodes(obs.Runner); len(got) != 0 {
		return errParityPremisef(obs.Case.Row,
			"runner injected %d learning episode(s) into %q, want 0: %s — that stage's contract is that NO "+
				"defect was established; an episode there is a fabricated, content-addressed defect signature "+
				"that gate.readEpisodeHistory would correlate into later runs (#3929)",
			len(got), "park-infrastructure-failure", joinLearningEpisodes(got))
	}
	return nil
}

// checkLearningEpisodeInfraForwardBranch grades the engine against the runner.
//
// The retry decision is compared FIRST and in full, because it is what proves
// the ruling was applied where it was supposed to be: #3929 gates the
// injection and nothing else, so the annotation — including its
// journal.AttemptInfra class and its repassAttempt 0 — must be byte-identical
// on both sides and must still be written. A change that gated the retry
// decision itself, or that stopped classifying the infra outcome as retryable,
// would also produce zero episodes on both sides and would otherwise pass.
func checkLearningEpisodeInfraForwardBranch(obs parityObservation) error {
	if err := diffRetryDecisions("engine", retryDecisions(obs.Engine), retryDecisions(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — the ruling withholds the INJECTION only; the infra classification, the annotation and the "+
				"branch taken are unchanged", err)
	}
	if got := learningEpisodes(obs.Engine); len(got) != 0 {
		return errParityRow(obs.Case.Row,
			"engine injected %d learning episode(s) where the runner injected none: %s",
			len(got), joinLearningEpisodes(got))
	}
	if err := diffLearningPointers(obs); err != nil {
		return err
	}
	// Nothing was downgraded. The episode pointer is derived and produced
	// integrity is the floor of a stage's inputs, so an injection here would
	// show up as a derived park-infrastructure-failure — admission control
	// changing for a stage that received no correction.
	if err := requireNoDerivedStage(obs.Runner); err != nil {
		return errParityPremisef(obs.Case.Row, "runner: %v", err)
	}
	if err := requireNoDerivedStage(obs.Engine); err != nil {
		return errParityRow(obs.Case.Row, "engine: %v", err)
	}
	return checkAllSurfaces(obs)
}

// requireNoDerivedStage fails if any stage finished at derived integrity. On a
// fixture with no injection there is no derived pointer to be the floor, so a
// derived grade means one was threaded in after all.
func requireNoDerivedStage(side paritySide) error {
	for _, e := range side.Events {
		if e.Type == journal.EventStageFinished && e.Integrity == apiv1.IntegrityDerived {
			return fmt.Errorf("stage %q finished at %q with no injection anywhere in the walk; the downgrade "+
				"must be a consequence of an injected pointer, not of evaluating a gate",
				e.Stage, apiv1.IntegrityDerived)
		}
	}
	return nil
}
