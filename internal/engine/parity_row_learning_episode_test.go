package engine

// Parity rows E10-learning-episode-injection and E10-learning-episode-not-injected
// — plan item E10 (#3913).
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
// internal/runner's retry arm has a second route for the pointer:
// parallel.recordCurrentPointer, which scopes it to the active branch instead
// of the run-level pointer set. There is deliberately no engine counterpart and
// no parity row for it — spec.parallels is REFUSED at run start on the engine
// (registryrefusal.go, ruling R9), so the engine walk can never reach a retry
// arm inside a branch — and a parity row is impossible for the same reason:
// the engine side of such a fixture never starts. The claim is pinned as an
// executable one instead, by TestLearningEpisodeParallelRouteIsRunnerOnly in
// learningepisode_test.go, which exercises the runner's branch-scoped route and
// asserts the engine's refusal beside it. If parallels ever start walking on
// the engine, that test goes red and the branch-scoped route must be ported
// with it.

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

	// The row this table DISCOVERED while pinning the two above.
	//
	// A gate's fail branch does not have to send a stage BACK. It can route
	// onward, to a stage that has not run — the abort/park shape every lane
	// uses. The local runner's retry arm does not distinguish the two: its
	// guard (routeRetryDecision) asks only "retryable, non-pass, non-escalated,
	// real target", so a forward branch on a retryable code injects an episode
	// into a stage that never failed. The engine's port asks one question more
	// (gateSendsBack: is the target already completed?) and does not.
	//
	// Which side is right is a RULING, not a test: the runner's injection names
	// nextAttempt = sourceAttempt + 1 for a stage whose attempt is 1, and hands
	// a "correction" to work that was never corrected — but the parity contract
	// is that the engine reproduces the runner, and #3913's disposition for the
	// generic arm was "port the whole arm". So the divergence is stated here,
	// not resolved here: an expected failure with the reason on the row.
	//
	// The premise deliberately asserts the RUNNER still injects, so the day the
	// runner is fixed instead this row goes vacuous and reports as a harness
	// bug rather than silently passing.
	registerParityRow(parityCase{
		Row:        rowLearningEpisodeForwardBranch,
		Name:       "a retryable failure whose fail branch routes ONWARD injects on the runner only",
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
			// row from the negative one above: there the class declines the
			// failure, so neither side injects and the rows agree. Here the
			// class accepts it and only the target differs.
			"implement":   {fail("nonzero_exit", "3 tests failed")},
			"park-failed": {succeed(map[string]interface{}{"parked": "true"})},
		},
		Premise: premiseLearningEpisodeForwardBranch,
		Check:   checkLearningEpisodeInjectionCount,
	})
}

// premiseLearningEpisodeForwardBranch pins the runner behaviour the row is
// about: the fail branch really does route ONWARD (park-failed never ran
// before the gate), and the runner really does inject there anyway.
func premiseLearningEpisodeForwardBranch(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Runner, []string{"implement", "park-failed"}); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — this row is about a fail branch that routes ONWARD rather than re-entering", err)
	}
	got := learningEpisodes(obs.Runner)
	if len(got) != 1 {
		return errParityPremisef(obs.Case.Row,
			"runner injected %d learning episode(s) on a forward fail branch, want exactly 1: %s — if the "+
				"runner has stopped injecting here, this row's divergence is closed from the OTHER side and "+
				"the expected-failure entry must go with it",
			len(got), joinLearningEpisodes(got))
	}
	if got[0].Target != "park-failed" {
		return errParityPremisef(obs.Case.Row,
			"runner's injected episode targets %q, want the forward stage %q", got[0].Target, "park-failed")
	}
	return nil
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
		"artifact=%s recorded=%t pointer=%s stage=%s target=%s gate=%s sourceSeq=%d sourceAttempt=%d "+
			"nextAttempt=%d id=%s digest=%s path=%s signature=%q classification=%s action=%s "+
			"correction=%q findings=[%s] integrity=%s",
		r.ArtifactName, r.ArtifactRecorded, r.PointerName, r.Stage, r.Target, r.Gate, r.SourceSeq,
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
