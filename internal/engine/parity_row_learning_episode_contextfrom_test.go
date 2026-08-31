package engine

// Parity row E10-learning-episode-contextfrom — #3928.
//
// Inventory row (finding 002), a sub-case of the E10 producer row: "the
// injected learning.episode[<seq>] pointer must survive the re-entered stage's
// contextFrom selection, on both runners."
//
// # Why the existing E10 rows could not see this
//
// parityLearningEpisodeSpec builds detTask("implement", "review"), which
// declares NO contextFrom — and with an empty source list
// apiv1.SelectContextPointers is the identity function. Every other
// learning-episode fixture in the tree has the same shape, so the entire E10
// surface was pinned on the one configuration in which the selector cannot
// drop anything.
//
// The flagship implementation lane is not that shape. Its implement stage
// declares contextFrom (reference-workflows/gaggles/goobers/workflows/
// implementation.yaml), and before #3928 the selector kept only
// "<source>.verdict" and "<source>.artifact[..." — so the episode a repassing
// gate had just minted for that very stage was discarded before dispatch, and
// before ValidateInputIntegrity could grade it. Both runners call the selector
// as the first statement of their task entry point (internal/runner/run.go,
// internal/engine/engine.go), so the two sides agreed perfectly while both
// being wrong: parity alone can never catch a shared-helper defect, which is
// exactly why this row's PREMISE asserts delivery on the runner rather than
// only comparing the sides.
//
// This row is therefore doing two jobs: pinning the fix on the runner (the
// premise) and pinning that the engine reproduces it (the check).

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

// parityLearningEpisodeContextFromSpec is parityLearningEpisodeSpec with the
// implementation lane's shape on the re-entered stage: a contextFrom naming
// its own producers and its gate, and the maintainer minimum the lane declares
// — which the derived episode has to satisfy for the repass to be admitted at
// all.
func parityLearningEpisodeContextFromSpec() apiv1.WorkflowSpec {
	implement := detTask("implement", "review")
	implement.ContextFrom = []string{"implement", "review"}
	implement.MinimumIntegrity = apiv1.IntegrityMaintainer
	return fixtureSpec("implement",
		[]apiv1.Task{implement},
		[]apiv1.Gate{statusGate("review", map[string]string{
			"pass": wf.TerminalComplete,
			"fail": "implement",
		})},
	)
}

func init() {
	registerParityRow(parityCase{
		Row:        rowLearningEpisodeContextFrom,
		Name:       "a repassed stage that declares contextFrom still receives its learning episode on both runners",
		DSLVersion: "2.0",
		Spec:       parityLearningEpisodeContextFromSpec(),
		Script: map[string][]scriptedCall{
			"implement": {
				fail("nonzero_exit", "3 tests failed"),
				succeed(map[string]interface{}{"tests": "green"}),
			},
		},
		Premise: premiseLearningEpisodeContextFrom,
		Check:   checkLearningEpisodeInjection,
	})
}

// premiseLearningEpisodeContextFrom is the anti-vacuity half, and it carries
// more weight than most: the defect this row exists for was SYMMETRIC, so a
// pure engine-vs-runner comparison would have been green throughout. The
// premise therefore asserts the runner's own delivery outright.
//
// It also asserts the fixture still declares contextFrom. Delete that one line
// from the spec and the row silently degenerates into a duplicate of
// E10-learning-episode-injection — green, and testing nothing this row was
// added for.
func premiseLearningEpisodeContextFrom(obs parityObservation) error {
	task, ok := taskNamed(obs.Case.Spec, "implement")
	if !ok || len(task.ContextFrom) == 0 {
		return errParityPremisef(obs.Case.Row,
			"the fixture's implement stage declares no contextFrom, so apiv1.SelectContextPointers is the "+
				"identity function and this row is a duplicate of %s", rowLearningEpisodeInjection)
	}
	if task.MinimumIntegrity != apiv1.IntegrityMaintainer {
		return errParityPremisef(obs.Case.Row,
			"the fixture's implement stage declares minimumIntegrity %q, want %q — selection runs BEFORE "+
				"admission, so a surviving pointer must also be a GRADED one",
			task.MinimumIntegrity, apiv1.IntegrityMaintainer)
	}
	if err := requireStagesDispatched(obs.Runner, []string{"implement", "implement"}); err != nil {
		return errParityPremisef(obs.Case.Row, "%v — the fail branch must re-enter the stage", err)
	}
	episodes := learningEpisodes(obs.Runner)
	if len(episodes) != 1 {
		return errParityPremisef(obs.Case.Row,
			"runner injected %d learning episode(s), want exactly 1: %s",
			len(episodes), joinLearningEpisodes(episodes))
	}
	pointers := learningPointers(obs.Runner)
	if len(pointers) != 2 || pointers[0] != "" || pointers[1] == "" {
		return errParityPremisef(obs.Case.Row,
			"runner's learning pointers per dispatch were %q, want none on the first dispatch and one on "+
				"the re-entered dispatch — a stage that declares contextFrom must still be handed the "+
				"correction its own gate minted for it (#3928)", strings.Join(pointers, " | "))
	}
	grades := stageFinishedIntegrity(obs.Runner, "implement")
	if len(grades) != 2 || grades[1] != apiv1.IntegrityDerived {
		return errParityPremisef(obs.Case.Row,
			"runner's implement stage.finished integrity was [%s], want the repass to grade derived — if "+
				"the episode were still being filtered out, the repass would grade as though it had never "+
				"received one", joinIntegrity(grades))
	}
	return nil
}

// taskNamed looks a task up in a fixture spec.
func taskNamed(spec apiv1.WorkflowSpec, name string) (apiv1.Task, bool) {
	for _, task := range spec.Tasks {
		if task.Name == name {
			return task, true
		}
	}
	return apiv1.Task{}, false
}
