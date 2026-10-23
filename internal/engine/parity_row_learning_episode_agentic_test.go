package engine

// Parity row E10-learning-episode-agentic-repass — #3942, the residual of the
// #3929 ruling that #3938 landed.
//
// # What this row is about
//
// #3929 ruled that a learning episode is injected IFF the branch is a true
// repass (repassAttempt >= 1), and #3938 hoisted that predicate into
// runner.LearningEpisodeAppliesToRepass so both drivers would read it the same
// way. What neither touched is the condition WRAPPING the call: the injection
// sat inside the retry-decision arm on both sides, so it silently also
// required `retryable` —
// internal/runner.retryFailureClassForGateResult, true only for an automated
// `status-equals` gate over `nonzero_exit`/`base_sync_conflict`, or for ANY
// gate resolving `infra`.
//
// An agentic reviewer's `needs-changes` is neither. The evaluator is not
// automated; the outcome is the verdict decision. So the classifier declined
// the canonical repass of the entire system, and BOTH drivers declined it in
// agreement — which is precisely why no row in this table went red, and why
// this row had to be added rather than discovered.
//
// # Why this fixture is shaped the way it is
//
// The subject stage is DETERMINISTIC and the gate is AGENTIC. That is not a
// convenience:
//
//   - An agentic subject would trip #415's empty-diff fast-fail. scriptedExec
//     writes no files, so the runner's real worktree reports an empty diff, and
//     an agentic stage that changed nothing fails its gate with no reviewer
//     dispatched at all (row E5-empty-diff-fast-fail next door). The fixture
//     would then measure the fast-fail rather than the injection.
//   - A deterministic subject under the same empty diff IS still reviewed
//     (row E5-empty-diff-deterministic-subject), so the reviewer really runs,
//     really returns needs-changes, and really sends the stage back.
//   - An empty diff never populates the #316 duplicate-diff memory on either
//     side (both guard on a non-empty digest), so the second pass is a genuine
//     repass rather than an UNCHANGED_REPASS escalation. That matters: an
//     escalated result would set gr.Escalated, which the injection predicate
//     excludes, and the row would go green for the wrong reason.
//
// The result is the exact shape the classifier refuses and the ruling accepts:
// a live, non-escalated, repassAttempt-1 branch whose failure class is not
// `retryable`. Before #3942 both sides injected nothing here.
//
// # What it does NOT claim
//
// The retry decision itself. `needs-changes` from an agentic gate is not
// retry-classifiable, so no `stage.retry.decision` annotation is written on
// either side — before or after the fix. The premise asserts that ABSENCE, so
// this row also pins the scope of the change: the episode was widened, the
// classifier was not. A port that started classifying reviewer verdicts as
// policy failures (and so started annotating them, and charging them against
// the infra/policy split priorRepassCause reads) fails here.

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func init() {
	registerParityRow(parityCase{
		Row:        rowLearningEpisodeAgenticRepass,
		Name:       "an agentic reviewer's needs-changes repass injects the same learning episode on both runners",
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
			"implement": {
				succeed(map[string]interface{}{"attempt": "1"}),
				succeed(map[string]interface{}{"attempt": "2"}),
			},
		},
		Verdicts: map[string][]apiv1.Verdict{
			"review": {
				{
					Decision:  apiv1.VerdictNeedsChanges,
					Summary:   "the parser still accepts empty input",
					Rationale: "reject an empty document before the token scan rather than after it",
				},
				{
					Decision:  apiv1.VerdictPass,
					Summary:   "empty input is rejected now",
					Rationale: "the guard is in the right place and covered",
				},
			},
		},
		EngineWorkspaceDiffs: map[string][]byte{"review": nil},
		Premise:              premiseLearningEpisodeAgenticRepass,
		Check:                checkLearningEpisodeAgenticRepass,
	})
}

// premiseLearningEpisodeAgenticRepass is the anti-vacuity half, and it is where
// this row's whole value sits: before #3942 EVERY assertion below failed on the
// runner, with the engine agreeing, so the check would have graded two empty
// lists green.
//
// It pins, in order: the reviewer really ran twice and really sent the stage
// back; the branch was NOT escalated (so the predicate's escalation guard is
// not what is being measured); no retry decision was written (the classifier
// still declines a reviewer verdict — the scope claim); and the runner injected
// exactly one derived episode into the re-entered dispatch.
func premiseLearningEpisodeAgenticRepass(obs parityObservation) error {
	if err := requireStagesDispatched(obs.Runner, []string{"implement", "review", "implement", "review"}); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — a needs-changes verdict must re-enter the implementer for this row to be about a repass", err)
	}
	if n := countReviewerDispatches(obs.Runner, "review"); n != 2 {
		return errParityPremisef(obs.Case.Row,
			"the runner dispatched the reviewer %d time(s), want 2 — the fixture must actually reach an "+
				"agentic verdict on both passes, or it is measuring an automated gate in disguise", n)
	}
	if obs.Runner.Terminal.Status != string(StatusCompleted) {
		return errParityPremisef(obs.Case.Row,
			"the runner ended %q, want completed — an escalated run sets gr.Escalated, which the injection "+
				"predicate excludes, so the row would go green for the wrong reason",
			obs.Runner.Terminal.Status)
	}
	// The scope claim: widening the episode must not widen the CLASSIFIER.
	if got := retryDecisions(obs.Runner); len(got) != 0 {
		return errParityPremisef(obs.Case.Row,
			"the runner wrote %d retry-decision annotation(s) for an agentic needs-changes verdict, want 0 "+
				"— #3942 widened the EPISODE only; retryFailureClassForGateResult still declines a reviewer "+
				"verdict, and a policy/infra class asserted over one would corrupt what priorRepassCause "+
				"reads back: %+v", len(got), got)
	}
	got := learningEpisodes(obs.Runner)
	if len(got) != 1 {
		return errParityPremisef(obs.Case.Row,
			"runner injected %d learning episode(s), want exactly 1: %s — this is the #3942 gap: an agentic "+
				"reviewer's needs-changes is the canonical true repass, and the retry classifier's veto used "+
				"to withhold the correction from it",
			len(got), joinLearningEpisodes(got))
	}
	record := got[0]
	if !record.ArtifactRecorded {
		return errParityPremisef(obs.Case.Row,
			"runner annotated episode %q but recorded no %s artifact", record.EpisodeID, record.ArtifactName)
	}
	if record.Integrity != apiv1.IntegrityDerived {
		return errParityPremisef(obs.Case.Row,
			"runner's episode annotation reads integrity %q, want %q", record.Integrity, apiv1.IntegrityDerived)
	}
	if record.Target != "implement" || record.Gate != "review" {
		return errParityPremisef(obs.Case.Row,
			"runner's episode targets %s via gate %s, want implement via review — the correction must be "+
				"addressed to the stage actually being re-entered", record.Target, record.Gate)
	}
	// The reviewer's own words must reach the repass. A synthesized fallback
	// here would mean the verdict arm of BuildLearningEpisode was bypassed,
	// which is the difference between a correction and a restatement.
	if record.Correction == "" {
		return errParityPremisef(obs.Case.Row,
			"runner's episode carries no correctionFeedback; the reviewer's rationale is the payload the "+
				"repass is supposed to argue with")
	}
	if err := requireRunnerLearningPointerOnRepass(obs); err != nil {
		return err
	}
	grades := stageFinishedIntegrity(obs.Runner, "implement")
	if len(grades) != 2 || grades[0] != apiv1.IntegrityTrusted || grades[1] != apiv1.IntegrityDerived {
		return errParityPremisef(obs.Case.Row,
			"runner's implement stage.finished integrity was [%s], want [trusted derived] — the injected "+
				"pointer is derived and produced integrity is the floor of a stage's inputs, so the repass "+
				"grades derived; that downgrade is admission control and is part of what this row compares",
			joinIntegrity(grades))
	}
	return nil
}

// requireRunnerLearningPointerOnRepass asserts the runner threaded the episode
// into the RE-ENTERED implement dispatch and into no earlier one.
//
// It selects by envelope stage rather than by dispatch index because this
// fixture's envelope stream interleaves reviewer dispatches with stage ones,
// unlike the deterministic E10 rows next door.
func requireRunnerLearningPointerOnRepass(obs parityObservation) error {
	pointers := learningPointers(obs.Runner)
	var implement []string
	for i, env := range obs.Runner.Envelopes {
		if env.Stage == "implement" && i < len(pointers) {
			implement = append(implement, pointers[i])
		}
	}
	if len(implement) != 2 || implement[0] != "" || implement[1] == "" {
		return errParityPremisef(obs.Case.Row,
			"runner's learning pointers on the implement dispatches were %v, want the first to carry none "+
				"and the repass to carry one", implement)
	}
	return nil
}

// checkLearningEpisodeAgenticRepass grades the engine against the runner on the
// episode record (whose ID, signature and digest are content-addressed over
// every episode field, so agreement there is agreement on all of them), the
// per-dispatch pointer sets, the repass integrity downgrade, the retry
// decisions, and the four shared surfaces.
func checkLearningEpisodeAgenticRepass(obs parityObservation) error {
	if err := diffLearningEpisodes("engine", learningEpisodes(obs.Engine), learningEpisodes(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — an agentic needs-changes repass owes the same correction on both drivers", err)
	}
	if err := diffRetryDecisions("engine", retryDecisions(obs.Engine), retryDecisions(obs.Runner)); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — #3942 widened the injection predicate, never the retry classifier", err)
	}
	if err := diffLearningPointers(obs); err != nil {
		return err
	}
	if err := diffRepassIntegrity(obs, "implement"); err != nil {
		return err
	}
	return checkAllSurfaces(obs)
}
