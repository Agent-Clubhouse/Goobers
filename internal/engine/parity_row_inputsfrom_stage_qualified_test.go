package engine

// Parity rows E2-inputsfrom-stage-qualified (EXPECTED FAILURE) and
// P0-inputsfrom-bare-key (must stay GREEN).
//
// Inventory row: "Stage-qualified inputsFrom (#562): `stage.key` resolves
// against any completed stage's outputs (with its integrity grade) when the DSL
// version supports it; bare keys resolve against the immediately preceding
// stage." Runner site: internal/runner/inputsfrom.go:78-112 with
// workflow.SupportsStageQualifiedInputs (machine.go:90-95). Engine:
// engine.go:555-561 resolves ONLY against upstreamResult.Outputs, so
// "<stage>.<key>" is looked up as a literal output key, is not found, and the
// walk fails closed.
//
// The pair is deliberate. The bare-key row is the far-side golden finding 002
// names for P0 ("the 'bare inputsFrom resolve identically' golden is green on
// both runners") and includes the LEGACY DOTTED KEY case — an output literally
// named "a.b" produced by the immediately preceding stage — which is precisely
// what the runner's resolution order is built to preserve. Without it, a port
// that "fixed" the stage-qualified row by always splitting on the first dot
// would break every legacy dotted key and this suite would not notice.
//
// Both fixtures are DSL 2.0 (backlog-curation's own version), which
// SupportsStageQualifiedInputs admits — the feature is live for every remaining
// DSL version, so this is not a preview-only shape.
//
// Closed by plan item E2: the engine carries a completed-stage output map as
// workflow state and resolves through the same order the runner uses. When it
// lands, DELETE the stage-qualified row's parityExpectedFailures entry.

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// parityInputsFromSpec builds an A -> B -> C chain whose final stage consumes
// one inputsFrom reference. A produces the value under `key`; B produces an
// unrelated output, so a resolver that only looks at the immediately preceding
// stage cannot find A's.
func parityInputsFromSpec(reference string) apiv1.WorkflowSpec {
	consume := apiv1.Task{
		Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
		Run:        &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		InputsFrom: map[string]string{"resolved": reference},
	}
	return apiv1.WorkflowSpec{
		Gaggle:   "goobers",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "produce",
		Tasks: []apiv1.Task{
			{
				Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: "intervene",
			},
			{
				Name: "intervene", Type: apiv1.TaskDeterministic, Goal: "intervene",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: "consume",
			},
			consume,
		},
	}
}

func init() {
	registerParityRow(parityCase{
		Row:        rowInputsFromStageQualified,
		Name:       "stage.key resolves against a non-adjacent completed stage",
		DSLVersion: "2.0",
		Spec:       parityInputsFromSpec("produce.selectedPr"),
		Script: map[string][]scriptedCall{
			"produce":   {succeed(map[string]interface{}{"selectedPr": "4242"})},
			"intervene": {succeed(map[string]interface{}{"unrelated": "noise"})},
		},
		// The engine fails the walk closed on the unresolvable reference
		// ("upstream output %q not found"), so its workflow errors while the
		// runner completes — itself a parity divergence, and the reason the
		// row needs its own Check rather than the default terminal diff.
		WantEngineErr: true,
		Check:         checkInputsFromStageQualified,
	})

	registerParityRow(parityCase{
		Row:        rowInputsFromBareKey,
		Name:       "bare and legacy dotted keys resolve from the preceding stage on both runners",
		DSLVersion: "2.0",
		Spec:       parityInputsFromSpec("legacy.dotted.key"),
		Script: map[string][]scriptedCall{
			"produce":   {succeed(map[string]interface{}{"selectedPr": "4242"})},
			"intervene": {succeed(map[string]interface{}{"legacy.dotted.key": "preserved"})},
		},
		Check: checkInputsFromBareKey,
	})
}

// checkInputsFromStageQualified asserts the runner really does resolve the
// non-adjacent reference (so the row is not vacuous) before diffing.
func checkInputsFromStageQualified(obs parityObservation) error {
	if err := requireEnvelopeInput(obs.Runner, "consume", "resolved", "4242"); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — #562 stage-qualified resolution is the behaviour this row pins", err)
	}
	if err := requireEnvelopeInput(obs.Engine, "consume", "resolved", "4242"); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}

// checkInputsFromBareKey is the golden: a dotted key whose prefix is NOT a
// stage stays a bare output key on both runners.
func checkInputsFromBareKey(obs parityObservation) error {
	for _, side := range []paritySide{obs.Runner, obs.Engine} {
		if err := requireEnvelopeInput(side, "consume", "resolved", "preserved"); err != nil {
			return errParityRow(obs.Case.Row,
				"%v — a legacy dotted output key must not be re-read as a stage reference", err)
		}
	}
	return checkAllSurfaces(obs)
}
