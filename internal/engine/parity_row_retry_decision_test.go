package engine

// Parity rows E2-retry-decision-annotation and E2-retry-decision-not-on-pass —
// CLOSED by plan item E2, must stay GREEN.
//
// Inventory row: "retry-decision annotation: when a gate's fail branch re-enters
// a completed stage, the runner writes a runner.annotation recording the failure
// CLASS (policy vs infrastructure), the subject's failure code, the cumulative
// repass attempt and the branch target — the record priorRepassCause reads back
// (E6). It also skips the checker entirely when the status-equals outcome is
// already known from the failure code (the knownOutcome shortcut)."
// Runner site: internal/runner/run.go's routeRetryDecision (~4288) and
// retryFailureClass (~4241). Engine: retrydecision.go's (*runJournal).retryDecision
// plus the knownOutcome arm of walk's gate step.
//
// # What this row can and cannot see, and why that shaped it
//
// The annotation lives in the runner.* namespace, and journal.IsConformanceNormative
// excludes that whole namespace by prefix. So it is invisible to
// diffConformanceViews — the surface every other row leans on. Asserting it
// therefore means reading the RAW event logs (paritySide.Events), which is what
// checkRetryDecisionAnnotation does. That is not a weaker assertion: the raw log
// is a superset of the conformance view, and comparing the annotations field by
// field is a tighter claim than "the normative projections agree".
//
// The knownOutcome shortcut is likewise invisible on the envelope surface,
// because neither runner routes its automated gate evaluator through
// recordingExec. What IS visible is its consequence — the AttemptClass the
// annotation carries — so the shortcut is pinned here through that, and
// separately, directly, by TestKnownOutcomeShortcutSkipsTheChecker in
// gateshortcut_test.go with a counting evaluator.
//
// # The learning-episode gap, and what this row therefore excludes
//
// This is the first row in the table to walk a gate FAIL branch back into a
// completed stage, and doing so surfaced a divergence that is real, is NOT
// E2's, and was not previously in the inventory: on that same retry arm the
// local runner also injects a learning episode — it records a
// learning/episode-<gate>-<seq>.json artifact and threads a
// learning.episode[<seq>] context pointer into the re-entered stage
// (internal/runner/run.go's recordLearningInjection, reachable only from the
// retry branch of stepGate). The engine has no counterpart, and cannot cheaply
// grow one: its walk has no artifact-recording seam at all, because artifacts
// are recorded during history projection rather than during the run.
//
// It is recorded in this package's drift ledger (doc.go) and reported on #3874
// as a candidate inventory row. It is NOT added to parityExpectedFailures: that
// map is keyed by finding-002 row id, and inventing an id for a row the
// inventory does not have would corrupt the join key the whole table is built
// on — the harness's own failure text says a newly discovered gap needs its
// inventory row first.
//
// What it costs this row is the envelope and conformance surfaces, which
// checkSurfacesForRetryRow excludes and explains. The three #415 rows next door
// were written to route around the retry path precisely so that they keep
// comparing all four surfaces in full; the exclusion is confined to the two rows
// that cannot avoid it, and checkLearningEpisodeGapIsBounded fails them if the
// gap ever stops being exactly runner-only.

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
)

// parityRetryDecisionSpec is implement -> review, whose fail branch re-enters
// implement. A status-equals gate is required: retryFailureClass refuses any
// other evaluator, so a fixture with a different check would silently stop
// exercising the shortcut.
func parityRetryDecisionSpec() apiv1.WorkflowSpec {
	return fixtureSpec("implement",
		[]apiv1.Task{detTask("implement", "review")},
		[]apiv1.Gate{statusGate("review", map[string]string{
			"pass": wf.TerminalComplete,
			"fail": "implement",
		})},
	)
}

func init() {
	registerParityRow(parityCase{
		Row:        rowRetryDecisionAnnotation,
		Name:       "a fail branch re-entering a stage writes the same retry decision on both runners",
		DSLVersion: "2.0",
		Spec:       parityRetryDecisionSpec(),
		Script: map[string][]scriptedCall{
			"implement": {
				// nonzero_exit is one of the two codes retryFailureClass
				// recognizes, so both the shortcut and the annotation apply.
				fail("nonzero_exit", "3 tests failed"),
				// base_sync_conflict is the other, and it classifies the same
				// way — a second repass keeps the attempt counter moving so
				// "repassAttempt" cannot pass by both sides reporting 1.
				fail(runner.BaseSyncConflictErrorCode, "base moved under the branch"),
				succeed(map[string]interface{}{"tests": "green"}),
			},
		},
		Premise: premiseRetryDecisionAnnotation,
		Check:   checkRetryDecisionAnnotation,
	})

	// The negative half. A PASSING gate is not a retry decision, and neither
	// is a failure whose outcome the shortcut cannot know — without this, a
	// port that annotated every gate evaluation would leave the positive row
	// green while filling E6's evidence channel with noise.
	registerParityRow(parityCase{
		Row:        rowRetryDecisionNotOnPass,
		Name:       "a passing gate and an unclassifiable failure write no retry decision",
		DSLVersion: "2.0",
		Spec: fixtureSpec("implement",
			[]apiv1.Task{
				detTask("implement", "review"),
				detTask("park-failed", wf.TargetAbort),
			},
			[]apiv1.Gate{statusGate("review", map[string]string{
				"pass": wf.TerminalComplete,
				"fail": "park-failed",
			})},
		),
		Script: map[string][]scriptedCall{
			// An unrecognized code: the gate is evaluated for real, fails,
			// and routes on — but retryFailureClass declines it, so there is
			// no retry decision to record.
			"implement":   {fail("assertion_failed", "an assertion tripped")},
			"park-failed": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseRetryDecisionNotOnPass,
		Check:   checkRetryDecisionNotOnPass,
	})
}

// retryDecision is one runner.annotation of kind stage.retry.decision, reduced
// to the fields routeRetryDecision writes, so the two sides compare as values.
type retryDecisionRecord struct {
	Stage         string
	Gate          string
	FailureClass  string
	FailureCode   string
	RepassAttempt int
	Target        string
}

func (d retryDecisionRecord) String() string {
	return fmt.Sprintf("stage=%s gate=%s class=%s failureCode=%s repassAttempt=%d target=%s",
		d.Stage, d.Gate, d.FailureClass, d.FailureCode, d.RepassAttempt, d.Target)
}

// retryDecisions extracts a side's retry-decision annotations in order.
//
// It keys on the annotation's own "kind" discriminator rather than on the event
// type, because runner.annotation is a shared envelope: learning-episode
// injection and placement provenance ride the same type. A reader that took
// every annotation would be comparing three unrelated features at once.
func retryDecisions(side paritySide) []retryDecisionRecord {
	var out []retryDecisionRecord
	for _, e := range side.Events {
		if e.Type != journal.EventRunnerAnnotation || e.Runner == nil {
			continue
		}
		if kind, _ := e.Runner["kind"].(string); kind != runner.RetryDecisionKind {
			continue
		}
		out = append(out, retryDecisionRecord{
			Stage:         e.Stage,
			Gate:          e.Gate,
			FailureClass:  annotationString(e.Runner, runner.RetryFailureClassKey),
			FailureCode:   annotationString(e.Runner, "failureCode"),
			RepassAttempt: annotationInt(e.Runner, "repassAttempt"),
			Target:        annotationString(e.Runner, "target"),
		})
	}
	return out
}

func annotationString(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

// annotationInt reads a numeric annotation field. The two sides reach it by
// different routes — the runner's journal round-trips through JSON (float64),
// the engine's in-process events keep their Go type — so both are accepted
// rather than letting an encoding artifact read as a parity divergence.
func annotationInt(m map[string]interface{}, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

// premiseRetryDecisionAnnotation is the anti-vacuity half: the RUNNER must
// really write two policy-classed retry decisions with a moving attempt
// counter. Without it, a fixture whose gate stopped failing would leave the
// row comparing two empty lists.
func premiseRetryDecisionAnnotation(obs parityObservation) error {
	got := retryDecisions(obs.Runner)
	want := []retryDecisionRecord{
		{Stage: "implement", Gate: "review", FailureClass: string(journal.AttemptPolicy),
			FailureCode: "nonzero_exit", RepassAttempt: 1, Target: "implement"},
		{Stage: "implement", Gate: "review", FailureClass: string(journal.AttemptPolicy),
			FailureCode: runner.BaseSyncConflictErrorCode, RepassAttempt: 2, Target: "implement"},
	}
	if err := diffRetryDecisions("runner", got, want); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — this row exists to compare retry decisions, so the runner must write them", err)
	}
	if err := requireStagesDispatched(obs.Runner, []string{"implement", "implement", "implement"}); err != nil {
		return errParityPremisef(obs.Case.Row, "%v — the fail branch must re-enter the stage", err)
	}
	return nil
}

// checkRetryDecisionAnnotation grades the engine's annotations against the
// runner's, then the shared surfaces minus the bounded learning-episode gap.
func checkRetryDecisionAnnotation(obs parityObservation) error {
	if err := diffRetryDecisions("engine", retryDecisions(obs.Engine), retryDecisions(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkSurfacesForRetryRow(obs)
}

// premiseRetryDecisionNotOnPass pins the runner's silence: an unclassifiable
// failure and the passing gate that follows write NO retry decision, even
// though the gate really was evaluated.
func premiseRetryDecisionNotOnPass(obs parityObservation) error {
	if got := retryDecisions(obs.Runner); len(got) != 0 {
		return errParityPremisef(obs.Case.Row,
			"runner wrote %d retry decision(s) for an unclassifiable failure, want 0: %s",
			len(got), joinRetryDecisions(got))
	}
	if countGateEvaluations(obs.Runner, "review") == 0 {
		return errParityPremisef(obs.Case.Row,
			"runner never evaluated gate %q — an unclassifiable failure must still be gated, "+
				"or this row proves nothing about which evaluations are annotated", "review")
	}
	return nil
}

func checkRetryDecisionNotOnPass(obs parityObservation) error {
	if got := retryDecisions(obs.Engine); len(got) != 0 {
		return errParityRow(obs.Case.Row,
			"engine wrote %d retry decision(s) where the runner wrote none: %s", len(got), joinRetryDecisions(got))
	}
	return checkAllSurfaces(obs)
}

func diffRetryDecisions(side string, got, want []retryDecisionRecord) error {
	if len(got) != len(want) {
		return fmt.Errorf("%s wrote %d retry decision(s), want %d:\n  got:  %s\n  want: %s",
			side, len(got), len(want), joinRetryDecisions(got), joinRetryDecisions(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("%s retry decision %d diverges:\n  got:  %s\n  want: %s",
				side, i+1, got[i], want[i])
		}
	}
	return nil
}

func joinRetryDecisions(decisions []retryDecisionRecord) string {
	if len(decisions) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(decisions))
	for _, d := range decisions {
		parts = append(parts, "["+d.String()+"]")
	}
	return strings.Join(parts, " ")
}

// --- the bounded learning-episode carve-out ---------------------------------

// learningEpisodeArtifactPrefix / learningEpisodePointerPrefix are how
// recordLearningInjection names the artifact it records and the pointer it
// threads into the re-entered stage.
const (
	learningEpisodeArtifactPrefix = "learning/episode-"
	learningEpisodePointerPrefix  = "learning.episode["
)

// checkSurfacesForRetryRow compares the surfaces this row is about — dispatch
// order, gate evaluations, walk outcome and terminal — and deliberately does
// NOT compare the envelope and conformance surfaces.
//
// That exclusion is the whole reason this function exists instead of
// checkAllSurfaces, so it is spelled out rather than assumed. The runner's
// learning-episode injection fires on exactly the arm this row walks, and it
// perturbs both excluded surfaces in three ways that all trace to the one
// cause:
//
//   - the envelope surface, because the re-entered stage is dispatched with a
//     learning.episode[<seq>] context pointer the engine does not have;
//   - the conformance surface, because recording the episode appends an
//     artifact.recorded event;
//   - the conformance surface AGAIN, one step removed, because that pointer is
//     derived-integrity and the stage's produced integrity is the floor of its
//     inputs — so the runner's re-entered stage.finished reads integrity=derived
//     where the engine's reads trusted.
//
// The third is why the exclusion is wholesale rather than a filter. Filtering
// the pointer and the artifact would leave the integrity difference behind, and
// "accept a runner-side integrity downgrade" is not a carve-out worth having: it
// is precisely the class of divergence the conformance surface exists to catch.
// Excluding the two surfaces outright, for one named reason, on the one row that
// has it, is narrower than teaching the shared differ to tolerate integrity
// drift.
//
// checkLearningEpisodeGapIsBounded then keeps the exclusion from rotting: the
// engine must emit no learning-episode events (or the gap has changed shape) and
// the runner must still emit some (or the exclusion is dead weight and should be
// deleted). Every other row in the table — including the three #415 rows, which
// were deliberately written to avoid the retry path for exactly this reason —
// still compares all four surfaces in full.
func checkSurfacesForRetryRow(obs parityObservation) error {
	if err := checkLearningEpisodeGapIsBounded(obs); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	if err := requireStagesDispatched(obs.Engine, stageOrder(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	if got, want := countGateEvaluations(obs.Engine, "review"), countGateEvaluations(obs.Runner, "review"); got != want {
		return errParityRow(obs.Case.Row, "engine evaluated gate %q %d time(s), runner %d", "review", got, want)
	}
	if err := diffParityWalkOutcome(obs); err != nil {
		return err
	}
	return diffParityTerminal(obs)
}

// stageOrder is the side's dispatch order, so the engine can be required to
// match the runner's rather than a hard-coded list the fixture could drift from.
func stageOrder(side paritySide) []string {
	out := make([]string, 0, len(side.Envelopes))
	for _, env := range side.Envelopes {
		out = append(out, env.Stage)
	}
	return out
}

// checkLearningEpisodeGapIsBounded is what keeps the exclusion above honest: the
// gap must still be the one it was written for — runner-only, and present.
func checkLearningEpisodeGapIsBounded(obs parityObservation) error {
	if n := countLearningEpisodeEvents(obs.Engine.Events); n != 0 {
		return fmt.Errorf("engine emitted %d learning-episode event(s); the surface exclusion assumes it emits "+
			"none, so the gap it covers has changed shape and the exclusion is no longer justified", n)
	}
	if countLearningEpisodeEvents(obs.Runner.Events) == 0 {
		return fmt.Errorf("runner emitted no learning-episode events, so the surface exclusion is dead weight — " +
			"if recordLearningInjection no longer fires on a repass, delete it and use checkAllSurfaces")
	}
	limit := min(len(obs.Runner.Envelopes), len(obs.Engine.Envelopes))
	for i := 0; i < limit; i++ {
		runnerOnly, engineOnly := diffPointerNames(
			obs.Runner.Envelopes[i].ContextPointers, obs.Engine.Envelopes[i].ContextPointers)
		if len(engineOnly) > 0 {
			return fmt.Errorf("dispatch %d: engine carries context pointer(s) the runner does not: %s",
				i+1, strings.Join(engineOnly, " "))
		}
		for _, p := range runnerOnly {
			if !strings.HasPrefix(p, learningEpisodePointerPrefix) {
				return fmt.Errorf("dispatch %d: runner carries context pointer %q the engine does not, and it is "+
					"not the known learning-episode gap — the surface exclusion does not cover it", i+1, p)
			}
		}
	}
	return nil
}

func isLearningEpisodeEvent(e journal.Event) bool {
	if e.Type == journal.EventArtifactRecorded && strings.HasPrefix(e.Name, learningEpisodeArtifactPrefix) {
		return true
	}
	if e.Type == journal.EventRunnerAnnotation && e.Runner != nil {
		if kind, _ := e.Runner["kind"].(string); kind == "learning.episode.injected" {
			return true
		}
	}
	return false
}

func countLearningEpisodeEvents(events []journal.Event) int {
	n := 0
	for _, e := range events {
		if isLearningEpisodeEvent(e) {
			n++
		}
	}
	return n
}

// diffPointerNames splits two encodeParityPointers strings into the entries
// unique to each side. The ":kind:integrity" tail rides along in the returned
// strings so a failure message names the whole pointer.
func diffPointerNames(runnerPointers, enginePointers string) (runnerOnly, engineOnly []string) {
	inEngine := map[string]bool{}
	for _, p := range strings.Fields(enginePointers) {
		inEngine[p] = true
	}
	inRunner := map[string]bool{}
	for _, p := range strings.Fields(runnerPointers) {
		inRunner[p] = true
		if !inEngine[p] {
			runnerOnly = append(runnerOnly, p)
		}
	}
	for _, p := range strings.Fields(enginePointers) {
		if !inRunner[p] {
			engineOnly = append(engineOnly, p)
		}
	}
	return runnerOnly, engineOnly
}
