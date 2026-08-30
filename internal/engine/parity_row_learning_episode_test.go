package engine

// Parity rows E10-learning-episode-injection, E10-learning-episode-not-injected
// and E10-learning-episode-forward-branch — plan item E10 (#3913). The
// production-shaped infrastructure sibling of the third lives next door in
// parity_row_learning_episode_infra_test.go.
//
// Inventory row (finding 002): "Learning-episode injection on the generic gate
// retry arm (recordLearningInjection): a repassing gate records a
// learning/episode-<gate>-<seq>.json artifact, threads a learning.episode[<seq>]
// derived-integrity context pointer into the re-entered stage, and writes a
// learning.episode.injected runner.annotation carrying episodeId, signature,
// classification, recommendedAction, findingIdentities and correctionFeedback.
// Lane-agnostic: learningFindingsForRepass has an explicit non-verdict fallback,
// so it fires for deterministic stages too."
// Runner site: internal/runner/run.go's retry arm (~2091) and
// recordLearningInjection (~2123). Engine: (*runJournal).learningEpisode on the
// recordWalkArtifact seam, called from walk's gate arm.
//
// # Why this is its own row rather than E5's, and what it compares
//
// The producer half of the learning behaviour sits on the GENERIC retry arm —
// `if retry { … }` after routeRetryDecision, with no lane, executor-type or
// verdict condition — while E5/#3882's reconcileLearningFindings and
// disproveReviewerFindings are verdict-gated consumers. This row therefore
// walks a purely DETERMINISTIC fixture: if the injection only reproduced behind
// a reviewer, the split would be wrong.
//
// It compares the three surfaces #3913 measured as divergent, plus the walk:
//
//   - ENVELOPE: the re-entered stage carries learning.episode[<seq>], with the
//     same pointer NAME (which encodes the corrected event's journal sequence)
//     and the same derived integrity grade on both sides.
//   - CONFORMANCE: artifact.recorded for learning/episode-<gate>-<seq>.json.
//   - CONFORMANCE, one step removed and the one that actually changes routing:
//     the re-entered stage's stage.finished INTEGRITY. The injected pointer is
//     derived and produced integrity is the floor of a stage's inputs, so the
//     repassed stage grades derived rather than trusted. minimumIntegrity is
//     admission control, so a side that skipped the pointer would make a
//     different admission decision for the same definition on the same failure
//     — asserted explicitly here rather than left to ride along inside the
//     conformance diff, because that is the failure mode worth naming.
//
// checkAllSurfaces covers the first two (the harness's shared differs); the
// third and the episode's own content-addressed identity are compared field by
// field below, off the RAW event log, because runner.annotation lives in the
// runner.* namespace and journal.IsConformanceNormative excludes that whole
// namespace by prefix.
//
// # Parallels
//
// internal/runner's retry arm has TWO further routes for the pointer, both
// branch-scoped: parallel.recordCurrentPointer, taken when stepGate runs inside
// a sequentially-executed branch, and runBranch's own accumulator, taken when
// maxConcurrentBranches > 1. There is deliberately no engine counterpart and no
// parity row for either — spec.parallels is REFUSED at run start on the engine
// (registryrefusal.go, ruling R9), so the engine walk can never reach a retry
// arm inside a branch — and a parity row is impossible for the same reason:
// the engine side of such a fixture never starts. The claim is pinned as an
// executable one instead, by TestLearningEpisodeParallelRouteIsRunnerOnly in
// learningepisode_test.go, which exercises the runner's branch-scoped route and
// asserts the engine's refusal beside it. If parallels ever start walking on
// the engine, that test goes red and the branch-scoped routes must be ported
// with it.
//
// #3932 closed the divergence BETWEEN those two runner routes. runBranch used
// to carry a hand-copied half of the arm — the verdict pointer and not the
// learning episode — so maxConcurrentBranches, a scheduling bound, decided
// whether a repass received its correction. Both walkers now share one producer
// (runner.recordGateRetryInjection), and the equivalence is pinned by
// TestConcurrentAndSequentialBranchesInjectTheSameCorrection. That removes the
// prerequisite this note used to record for lifting R9: there is now one arm to
// port rather than two that disagree.
//
// # The forward-branch ruling (#3929)
//
// #3917 registered E10-learning-episode-forward-branch as an expected failure:
// on a retryable failure whose gate branch routes ONWARD to a stage that has
// never run, the runner injected an episode and the engine did not. The ruling
// that settled it is that the engine was right for the wrong reason, and the
// right reason is now spelled once, in runner.LearningEpisodeAppliesToRepass:
// an episode is injected IFF the gate result's repass attempt is at least 1.
//
// The engine used to answer the same question with its own re-derived
// gateSendsBack predicate (implementationlane.go), which happened to agree with
// the repass attempt at that call site but was a second derivation of a fact
// the gate had already computed and journalled. It is gone; both drivers now
// read the evidenced attempt. Nothing else moved: retry classification, the
// stage.retry.decision annotation (still written, still carrying repassAttempt
// 0 on a forward branch), routeRetryDecision's return, the routing itself and
// parallel failure accounting are all untouched — which is what the E2 rows
// and internal/runner's own forward-branch regressions pin.

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
)

// parityLearningEpisodeSpec is implement -> review, whose fail branch re-enters
// implement, with a deterministic implementer and an automated status-equals
// gate: no reviewer, no verdict, no agentic lane anywhere in the fixture.
func parityLearningEpisodeSpec() apiv1.WorkflowSpec {
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
		Row:        rowLearningEpisodeInjection,
		Name:       "a gate retry injects the same learning episode, pointer and integrity downgrade on both runners",
		DSLVersion: "2.0",
		Spec:       parityLearningEpisodeSpec(),
		Script: map[string][]scriptedCall{
			"implement": {
				fail("nonzero_exit", "3 tests failed"),
				succeed(map[string]interface{}{"tests": "green"}),
			},
		},
		Premise: premiseLearningEpisodeInjection,
		Check:   checkLearningEpisodeInjection,
	})

	// The negative half. A failure the retry classifier declines is NOT a
	// retry, so neither side may inject anything — without this, a port that
	// injected an episode on every gate evaluation would leave the positive
	// row green while filling every repass envelope with derived pointers and
	// silently downgrading stages that were never repassed.
	registerParityRow(parityCase{
		Row:        rowLearningEpisodeNotInjected,
		Name:       "a gate that does not retry injects no learning episode on either runner",
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
			// An unrecognized code: the gate is evaluated for real, fails, and
			// routes on to a DIFFERENT stage — routeRetryDecision declines it,
			// so there is no retry arm and no injection.
			"implement":   {fail("assertion_failed", "an assertion tripped")},
			"park-failed": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseLearningEpisodeNotInjected,
		Check:   checkLearningEpisodeNotInjected,
	})

	// The row this table DISCOVERED while pinning the two above, and the row
	// #3929's ruling closed.
	//
	// A gate's fail branch does not have to send a stage BACK. It can route
	// onward, to a stage that has not run — the park/disposition shape every
	// lane uses. The local runner's retry arm used not to distinguish the two:
	// its guard (routeRetryDecision) asks only "retryable, non-pass,
	// non-escalated, real target", so a forward branch on a retryable code
	// injected an episode into a stage that had never run. The engine asked one
	// question more and did not. That divergence was registered here as a
	// documented expected failure, because resolving it was a ruling.
	//
	// The ruling: an episode is injected IFF the branch is a true repass, read
	// off the gate result's own repassAttempt — the number the evaluator
	// already charged to the target's budget and the retry decision already
	// journaled. A forward branch has repassAttempt 0, so NEITHER side injects,
	// and the row states that as a positive claim.
	//
	// What the row must keep proving is that the arm is still LIVE. Zero
	// episodes on both sides is exactly what a fixture that stopped reaching
	// the retry arm at all would also report, so the premise asserts the retry
	// decision was really taken — the annotation, its policy class, and
	// repassAttempt 0 — and the check asserts that annotation still matches on
	// both sides. Without that this row would grade a broken fixture green.
	registerParityRow(parityCase{
		Row:        rowLearningEpisodeForwardBranch,
		Name:       "a retryable failure whose fail branch routes ONWARD injects on neither runner",
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
			// nonzero_exit IS retry-classifiable, which is what separates this
			// row from the negative one above: there the class DECLINES the
			// failure, so there is no retry arm at all and the row would be
			// green under either side of the ruling. Here the class ACCEPTS
			// it, the retry decision is really taken and really annotated, and
			// the only thing withheld is the injection.
			"implement":   {fail("nonzero_exit", "3 tests failed")},
			"park-failed": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseLearningEpisodeForwardBranch,
		Check:   checkLearningEpisodeForwardBranch,
	})

	// The NONTRIVIAL send-back (#3931). Everything above sends work back to
	// the stage that produced the failure, where the subject's attempt and the
	// target's next attempt are the same number and a derivation off the wrong
	// stage cannot be seen. This one separates them.
	registerParityRow(parityCase{
		Row:        rowLearningEpisodeSendBack,
		Name:       "a send-back to a stage OTHER than the failing one addresses the target's own next attempt on both runners",
		DSLVersion: "2.0",
		Spec:       parityLearningEpisodeSendBackSpec(),
		Script: map[string][]scriptedCall{
			"implement": {
				fail("nonzero_exit", "3 tests failed"),
				succeed(map[string]interface{}{"tests": "green"}),
				succeed(map[string]interface{}{"tests": "green"}),
			},
			"local-ci": {
				fail("nonzero_exit", "local ci is red"),
				succeed(map[string]interface{}{"ci": "green"}),
			},
		},
		Premise: premiseLearningEpisodeSendBack,
		Check:   checkLearningEpisodeSendBack,
	})
}

// parityLearningEpisodeSendBackSpec is the shipped implementation lane's
// skeleton: two gates whose fail branches BOTH re-enter implement, the second
// of them over a different subject.
//
//	implement -> review     {pass: local-ci,   fail: implement}
//	local-ci  -> local-gate {pass: @complete,  fail: implement}
//
// Scripted so both send-backs are taken, the walk is:
//
//	implement#1 fails    -> review fails     -> implement   (subject implement@1)
//	implement#2 succeeds -> review passes
//	local-ci#1  fails    -> local-gate fails -> implement   (subject local-ci@1)
//	implement#3 succeeds -> review passes    -> local-ci#2 succeeds -> complete
//
// The first send-back is trivial and pins that #3931 did not move the
// degenerate case. The second is the row's point: local-ci runs once per cycle
// so its attempt is 1, while implement is about to take its THIRD entry.
func parityLearningEpisodeSendBackSpec() apiv1.WorkflowSpec {
	return fixtureSpec("implement",
		[]apiv1.Task{
			detTask("implement", "review"),
			detTask("local-ci", "local-gate"),
		},
		[]apiv1.Gate{
			statusGate("review", map[string]string{
				"pass": "local-ci",
				"fail": "implement",
			}),
			statusGate("local-gate", map[string]string{
				"pass": wf.TerminalComplete,
				"fail": "implement",
			}),
		},
	)
}

// premiseLearningEpisodeSendBack pins the runner behaviour the row grades the
// engine against: two episodes, the first trivial and unchanged, the second
// addressed to a target attempt the SUBJECT's counter cannot name.
//
// The anti-vacuity assertion here is the stage-dispatch one. A fixture that
// stopped taking the second send-back — a changed repass budget, a changed
// classifier, a changed script — would report one episode instead of two and
// the interesting half of the row would silently disappear.
func premiseLearningEpisodeSendBack(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Runner,
		[]string{"implement", "implement", "local-ci", "implement", "local-ci"}); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — the row needs BOTH send-backs taken, so that implement's entry count overtakes the "+
				"subject's attempt", err)
	}
	got := learningEpisodes(obs.Runner)
	if len(got) != 2 {
		return errParityPremisef(obs.Case.Row,
			"runner injected %d learning episode(s), want exactly 2: %s", len(got), joinLearningEpisodes(got))
	}
	trivial, nontrivial := got[0], got[1]
	if trivial.Subject != "implement" || trivial.Target != "implement" ||
		trivial.SourceAttempt != 1 || trivial.NextAttempt != 2 {
		return errParityPremisef(obs.Case.Row,
			"the trivial send-back produced %s, want subject implement@1 addressed to implement/2 — "+
				"#3931 must not move the degenerate case", trivial)
	}
	if nontrivial.Subject != "local-ci" || nontrivial.Target != "implement" {
		return errParityPremisef(obs.Case.Row,
			"the second episode is %s, want subject local-ci sent back to implement — the whole row is "+
				"about a target that is not the failing stage", nontrivial)
	}
	if nontrivial.SourceAttempt != 1 {
		return errParityPremisef(obs.Case.Row,
			"the second episode's sourceAttempt is %d, want 1 — it names the SUBJECT's attempt, which is "+
				"what says which failure the episode is about, and #3931 does not move it",
			nontrivial.SourceAttempt)
	}
	if nontrivial.NextAttempt != 3 {
		return errParityPremisef(obs.Case.Row,
			"the second episode is addressed to implement/%d, want implement/3 — local-ci is on attempt 1 "+
				"while implement is about to take its third entry; sourceAttempt+1 says 2, an entry of "+
				"implement that has already happened with different content (#3931)",
			nontrivial.NextAttempt)
	}
	if nontrivial.SourceSeq == trivial.SourceSeq {
		return errParityPremisef(obs.Case.Row,
			"both episodes name source sequence %d — they correct different failures and must address "+
				"different events", nontrivial.SourceSeq)
	}
	// The annotation is TARGET-scoped (#3931). Its Stage and Attempt name the
	// entry the correction feeds, not the failure it is about — which is what
	// makes it findable from the dispatch that reads it, and why sourceStage
	// exists to name the subject.
	if nontrivial.Stage != "implement" {
		return errParityPremisef(obs.Case.Row,
			"the second episode's annotation is filed under stage %q, want implement — the annotation is "+
				"scoped to the target it corrects, so a nontrivial send-back files it against the stage "+
				"being re-entered rather than the one that failed", nontrivial.Stage)
	}
	grades := stageFinishedIntegrity(obs.Runner, "implement")
	if len(grades) != 3 || grades[0] != apiv1.IntegrityTrusted ||
		grades[1] != apiv1.IntegrityDerived || grades[2] != apiv1.IntegrityDerived {
		return errParityPremisef(obs.Case.Row,
			"runner's implement stage.finished integrity was [%s], want [trusted derived derived] — both "+
				"re-entries are graded on an injected correction", joinIntegrity(grades))
	}
	return nil
}

// checkLearningEpisodeSendBack grades the engine against the runner. Because
// learningEpisodeRecord carries the episode's content-addressed digest, an
// engine that derived the target attempt differently — or that read it off the
// subject, which is what the Temporal DefaultVersion branch still does for
// pre-#3931 histories — fails on the digest before it fails on the number.
func checkLearningEpisodeSendBack(obs parityObservation) error {
	if err := diffLearningEpisodes("engine", learningEpisodes(obs.Engine), learningEpisodes(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — both drivers must derive the target's next attempt from the TARGET's own history "+
				"through the shared builder (#3931)", err)
	}
	if err := diffLearningPointers(obs); err != nil {
		return err
	}
	if err := diffRepassIntegrity(obs, "implement"); err != nil {
		return err
	}
	return checkAllSurfaces(obs)
}

// premiseLearningEpisodeForwardBranch pins the runner behaviour the row is
// about: the fail branch really does route ONWARD (park-failed never ran
// before the gate), the retry arm really was ENTERED (a policy-classed retry
// decision naming the forward target, at repassAttempt 0 — the value #3929's
// predicate reads), and the runner injects nothing there.
//
// The middle assertion is the anti-vacuity one and it is the whole reason this
// row is distinct from E10-learning-episode-not-injected. Both rows observe
// zero episodes; only this one observes zero episodes on a branch the retry
// classifier ACCEPTED. If the fixture ever stopped producing a retry decision
// — a changed classifier, a changed gate, a changed failure code — the two
// rows would collapse into the same claim and this one would be measuring
// nothing.
func premiseLearningEpisodeForwardBranch(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Runner, []string{"implement", "park-failed"}); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — this row is about a fail branch that routes ONWARD rather than re-entering", err)
	}
	want := []retryDecisionRecord{{
		Stage: "implement", Gate: "review", FailureClass: string(journal.AttemptPolicy),
		FailureCode: "nonzero_exit", RepassAttempt: 0, Target: "park-failed",
	}}
	if err := diffRetryDecisions("runner", retryDecisions(obs.Runner), want); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — this row's claim is that a retry-CLASSIFIABLE forward branch injects nothing, so the "+
				"runner must still take the retry decision (at repassAttempt 0, which is the value #3929's "+
				"predicate reads); a fixture that stopped reaching the retry arm would report zero episodes "+
				"for the wrong reason", err)
	}
	if got := learningEpisodes(obs.Runner); len(got) != 0 {
		return errParityPremisef(obs.Case.Row,
			"runner injected %d learning episode(s) into a stage that has never run, want 0: %s — #3929 "+
				"ruled that an episode is injected iff the branch is a true repass (repassAttempt >= 1); a "+
				"forward branch is a disposition, and a stage that has not run has produced nothing to correct",
			len(got), joinLearningEpisodes(got))
	}
	return nil
}

// checkLearningEpisodeForwardBranch grades the engine against the runner: the
// same (empty) injection, the same retry decision, and all four shared
// surfaces. The retry-decision comparison is the load-bearing half — it is
// what proves the two sides took the same live retry arm and then both
// declined to inject, rather than one of them never getting there.
func checkLearningEpisodeForwardBranch(obs parityObservation) error {
	if err := diffRetryDecisions("engine", retryDecisions(obs.Engine), retryDecisions(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — the ruling gates the INJECTION only; retry classification, the annotation and the branch "+
				"taken are unchanged on both sides", err)
	}
	return checkLearningEpisodeInjectionCount(obs)
}

// checkLearningEpisodeInjectionCount compares the injections themselves. It is
// the injection row's check without the repass-specific integrity assertion,
// which has no meaning for a stage that is running for the first time.
func checkLearningEpisodeInjectionCount(obs parityObservation) error {
	if err := diffLearningEpisodes("engine", learningEpisodes(obs.Engine), learningEpisodes(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	if err := diffLearningPointers(obs); err != nil {
		return err
	}
	return checkAllSurfaces(obs)
}

// --- the episode record -----------------------------------------------------

// learningEpisodeRecord is one learning.episode.injected annotation plus the
// artifact it references, reduced to the fields the injection is ABOUT, so the
// two sides compare as values.
//
// EpisodeID, Signature and Digest are in the comparison on purpose: they are
// content-addressed over the whole episode (schema, run identity, workflow
// digest, normalized findings, evidence, correction feedback), so comparing
// them is a comparison of every episode field at once. Two sides that agree
// here cannot be building different episodes. That is also exactly why the
// construction is SHARED (internal/runner.BuildLearningEpisode) rather than
// mirrored — a second implementation would diverge silently, and this row would
// be the only thing standing between that and production.
type learningEpisodeRecord struct {
	ArtifactName      string
	PointerName       string
	Stage             string
	Subject           string
	Target            string
	Gate              string
	SourceSeq         uint64
	SourceAttempt     int
	NextAttempt       int
	EpisodeID         string
	Digest            string
	Path              string
	Signature         string
	Classification    string
	RecommendedAction string
	Correction        string
	FindingIdentities string
	Integrity         apiv1.Integrity
	ArtifactRecorded  bool
}

func (r learningEpisodeRecord) String() string {
	return fmt.Sprintf(
		"artifact=%s recorded=%t pointer=%s stage=%s subject=%s target=%s gate=%s sourceSeq=%d sourceAttempt=%d "+
			"nextAttempt=%d id=%s digest=%s path=%s signature=%q classification=%s action=%s "+
			"correction=%q findings=[%s] integrity=%s",
		r.ArtifactName, r.ArtifactRecorded, r.PointerName, r.Stage, r.Subject, r.Target, r.Gate, r.SourceSeq,
		r.SourceAttempt, r.NextAttempt, r.EpisodeID, r.Digest, r.Path, r.Signature, r.Classification,
		r.RecommendedAction, r.Correction, r.FindingIdentities, r.Integrity)
}

// learningEpisodes extracts a side's injections in order.
//
// It keys on the annotation's own "kind" discriminator rather than on the event
// type, because runner.annotation is a shared envelope: the retry decision and
// placement provenance ride the same type.
func learningEpisodes(side paritySide) []learningEpisodeRecord {
	recorded := map[string]journal.Event{}
	for _, e := range side.Events {
		if e.Type == journal.EventArtifactRecorded &&
			strings.HasPrefix(e.Name, learningEpisodeArtifactPrefix) {
			recorded[e.Name] = e
		}
	}
	var out []learningEpisodeRecord
	for _, e := range side.Events {
		if e.Type != journal.EventRunnerAnnotation || e.Runner == nil {
			continue
		}
		if kind, _ := e.Runner["kind"].(string); kind != runner.LearningEpisodeInjectedKind {
			continue
		}
		sourceSeq := uint64(annotationInt(e.Runner, "sourceSeq"))
		artifact, ok := recorded[e.Name]
		record := learningEpisodeRecord{
			ArtifactName:      e.Name,
			PointerName:       runner.LearningEpisodePointerName(sourceSeq),
			Stage:             e.Stage,
			Subject:           annotationString(e.Runner, "sourceStage"),
			Target:            annotationString(e.Runner, "target"),
			Gate:              annotationString(e.Runner, "gate"),
			SourceSeq:         sourceSeq,
			SourceAttempt:     annotationInt(e.Runner, "sourceAttempt"),
			NextAttempt:       annotationInt(e.Runner, "nextAttempt"),
			EpisodeID:         annotationString(e.Runner, "episodeId"),
			Digest:            annotationString(e.Runner, "episodeDigest"),
			Path:              annotationString(e.Runner, "episodePath"),
			Signature:         annotationString(e.Runner, "signature"),
			Classification:    annotationString(e.Runner, "classification"),
			RecommendedAction: annotationString(e.Runner, "recommendedAction"),
			Correction:        annotationString(e.Runner, "correctionFeedback"),
			FindingIdentities: strings.Join(annotationStrings(e.Runner, "findingIdentities"), ","),
			Integrity:         e.Integrity,
			ArtifactRecorded:  ok,
		}
		// The annotation's own Ref must address the artifact the journal
		// actually committed — the whole point of the in-walk seam is that the
		// digest the pointer carries is the digest on disk.
		if ok && artifact.Ref != nil && e.Ref != nil && artifact.Ref.Digest != e.Ref.Digest {
			record.Digest = "annotation-ref-mismatch:" + e.Ref.Digest
		}
		out = append(out, record)
	}
	return out
}

// annotationStrings reads a []string annotation field. Like annotationInt it
// accepts both routes the two sides reach it by: the runner's journal
// round-trips through JSON ([]any of string), the engine's projected events
// likewise, and an in-process []string is accepted so the reader cannot become
// encoding-sensitive.
func annotationStrings(m map[string]interface{}, key string) []string {
	switch v := m[key].(type) {
	case []string:
		return append([]string(nil), v...)
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, _ := item.(string)
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}

func diffLearningEpisodes(side string, got, want []learningEpisodeRecord) error {
	if len(got) != len(want) {
		return fmt.Errorf("%s injected %d learning episode(s), want %d:\n  got:  %s\n  want: %s",
			side, len(got), len(want), joinLearningEpisodes(got), joinLearningEpisodes(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("%s learning episode %d diverges:\n  got:  %s\n  want: %s",
				side, i+1, got[i], want[i])
		}
	}
	return nil
}

func joinLearningEpisodes(records []learningEpisodeRecord) string {
	if len(records) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(records))
	for _, r := range records {
		parts = append(parts, "["+r.String()+"]")
	}
	return strings.Join(parts, " ")
}

// --- the injected pointers --------------------------------------------------

// learningPointers is the set of learning.episode[...] pointer entries a side
// dispatched, per dispatch, in the harness's own pointer encoding.
func learningPointers(side paritySide) []string {
	out := make([]string, 0, len(side.Envelopes))
	for _, env := range side.Envelopes {
		var carried []string
		for _, p := range strings.Fields(env.ContextPointers) {
			if strings.HasPrefix(p, learningEpisodePointerPrefix) {
				carried = append(carried, p)
			}
		}
		sort.Strings(carried)
		out = append(out, strings.Join(carried, " "))
	}
	return out
}

// stageFinishedIntegrity is the produced integrity grade of each stage.finished
// for stage, in order — the surface the injected pointer downgrades.
func stageFinishedIntegrity(side paritySide, stage string) []apiv1.Integrity {
	var out []apiv1.Integrity
	for _, e := range side.Events {
		if e.Type == journal.EventStageFinished && e.Stage == stage {
			out = append(out, e.Integrity)
		}
	}
	return out
}

func joinIntegrity(grades []apiv1.Integrity) string {
	parts := make([]string, 0, len(grades))
	for _, g := range grades {
		parts = append(parts, string(g))
	}
	return strings.Join(parts, " ")
}

// --- the positive row -------------------------------------------------------

// premiseLearningEpisodeInjection is the anti-vacuity half: the RUNNER must
// still inject exactly one episode on this repass, thread its derived pointer
// into the re-entered dispatch, and downgrade that dispatch's produced
// integrity. Without it, a fixture whose gate stopped failing — or a
// recordLearningInjection that quietly stopped firing for deterministic stages
// — would leave the row comparing two empty lists.
func premiseLearningEpisodeInjection(obs parityObservation) error {
	got := learningEpisodes(obs.Runner)
	if len(got) != 1 {
		return errParityPremisef(obs.Case.Row,
			"runner injected %d learning episode(s), want exactly 1: %s — this row exists to compare the "+
				"injection, so the runner must perform it on a DETERMINISTIC repass",
			len(got), joinLearningEpisodes(got))
	}
	record := got[0]
	if !record.ArtifactRecorded {
		return errParityPremisef(obs.Case.Row,
			"runner annotated episode %q but recorded no %s artifact", record.EpisodeID, record.ArtifactName)
	}
	if record.Integrity != apiv1.IntegrityDerived {
		return errParityPremisef(obs.Case.Row,
			"runner's episode annotation reads integrity %q, want %q — the downgrade this row measures "+
				"depends on the episode being derived", record.Integrity, apiv1.IntegrityDerived)
	}
	if err := requireStagesDispatched(obs.Runner, []string{"implement", "implement"}); err != nil {
		return errParityPremisef(obs.Case.Row, "%v — the fail branch must re-enter the stage", err)
	}
	pointers := learningPointers(obs.Runner)
	if len(pointers) != 2 || pointers[0] != "" || pointers[1] == "" {
		return errParityPremisef(obs.Case.Row,
			"runner's learning pointers per dispatch were %q, want the FIRST dispatch to carry none and the "+
				"re-entered dispatch to carry one", strings.Join(pointers, " | "))
	}
	grades := stageFinishedIntegrity(obs.Runner, "implement")
	if len(grades) != 2 || grades[0] != apiv1.IntegrityTrusted || grades[1] != apiv1.IntegrityDerived {
		return errParityPremisef(obs.Case.Row,
			"runner's implement stage.finished integrity was [%s], want [trusted derived] — the injected "+
				"pointer is what downgrades the repass, and this row is the evidence for that claim",
			joinIntegrity(grades))
	}
	return nil
}

// checkLearningEpisodeInjection grades the engine's injection against the
// runner's on all three measured surfaces, then all four shared surfaces.
func checkLearningEpisodeInjection(obs parityObservation) error {
	if err := diffLearningEpisodes("engine", learningEpisodes(obs.Engine), learningEpisodes(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	if err := diffLearningPointers(obs); err != nil {
		return err
	}
	if err := diffRepassIntegrity(obs, "implement"); err != nil {
		return err
	}
	return checkAllSurfaces(obs)
}

// diffLearningPointers compares the learning pointers per dispatch. It is a
// narrower claim than the envelope surface but a much clearer failure message:
// "dispatch 2 carries no learning pointer" beats a full envelope dump.
func diffLearningPointers(obs parityObservation) error {
	runnerPointers, enginePointers := learningPointers(obs.Runner), learningPointers(obs.Engine)
	if len(runnerPointers) != len(enginePointers) {
		return errParityRow(obs.Case.Row, "engine made %d dispatch(es), runner %d",
			len(enginePointers), len(runnerPointers))
	}
	for i := range runnerPointers {
		if runnerPointers[i] == enginePointers[i] {
			continue
		}
		stage := "?"
		if i < len(obs.Runner.Envelopes) {
			stage = obs.Runner.Envelopes[i].Stage
		}
		return errParityRow(obs.Case.Row,
			"dispatch %d (%s) learning pointers diverge:\n  runner: %q\n  engine: %q",
			i+1, stage, runnerPointers[i], enginePointers[i])
	}
	return nil
}

// diffRepassIntegrity compares the produced integrity of every attempt of
// stage. This is the surface #3913 calls "the one that matters most and is easy
// to miss": integrity is admission control, so two runners disagreeing here can
// admit different work for the same definition on the same failure.
func diffRepassIntegrity(obs parityObservation, stage string) error {
	runnerGrades := stageFinishedIntegrity(obs.Runner, stage)
	engineGrades := stageFinishedIntegrity(obs.Engine, stage)
	if len(runnerGrades) != len(engineGrades) {
		return errParityRow(obs.Case.Row, "engine finished stage %q %d time(s), runner %d",
			stage, len(engineGrades), len(runnerGrades))
	}
	for i := range runnerGrades {
		if runnerGrades[i] != engineGrades[i] {
			return errParityRow(obs.Case.Row,
				"stage %q attempt %d produced integrity %q on the engine and %q on the runner — "+
					"minimumIntegrity is admission control, so the two sides would admit different work",
				stage, i+1, engineGrades[i], runnerGrades[i])
		}
	}
	return nil
}

// --- the negative row -------------------------------------------------------

// premiseLearningEpisodeNotInjected pins the runner's silence: a gate that
// routes onward without a retry writes no episode, even though it really was
// evaluated and really did fail.
func premiseLearningEpisodeNotInjected(obs parityObservation) error {
	if got := learningEpisodes(obs.Runner); len(got) != 0 {
		return errParityPremisef(obs.Case.Row,
			"runner injected %d learning episode(s) for a non-retrying gate, want 0: %s",
			len(got), joinLearningEpisodes(got))
	}
	if countGateEvaluations(obs.Runner, "review") == 0 {
		return errParityPremisef(obs.Case.Row,
			"runner never evaluated gate %q — a declined failure must still be gated, or this row proves "+
				"nothing about which evaluations inject", "review")
	}
	return nil
}

func checkLearningEpisodeNotInjected(obs parityObservation) error {
	if got := learningEpisodes(obs.Engine); len(got) != 0 {
		return errParityRow(obs.Case.Row,
			"engine injected %d learning episode(s) where the runner injected none: %s",
			len(got), joinLearningEpisodes(got))
	}
	for i, carried := range learningPointers(obs.Engine) {
		if carried != "" {
			return errParityRow(obs.Case.Row,
				"engine dispatch %d carries learning pointer(s) %q on a walk with no retry arm", i+1, carried)
		}
	}
	return checkAllSurfaces(obs)
}
