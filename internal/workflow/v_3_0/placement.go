package v30

// placement.go builds the DSL 3.0 side of the shared constraint solver's
// input (internal/runnersolve, dsl-3.0.md §5): each stage's EFFECTIVE
// placement requirement — declared runsOn ∪ derived requirements ∪ the
// gaggle-level floor. This is the single derivation point all three
// admission checkpoints consume (via internal/workflow.StagePlacements), so
// apply-time validation, the boot pass, and the per-run admit can never
// disagree about what a 3.0 stage requires.

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/runnersolve"
)

// StagePlacements returns every placeable stage's effective placement
// requirement: every task, in task order, then every AGENTIC gate that
// declares runsOn, in gate order (decision 001 — agentic gates are
// placeable; the reviewer derives harness:<its goober's harness> and the
// gaggle floor merges exactly as it does for a task). goobers supplies the
// referenced goober specs for harness derivation (see DerivedCapabilities /
// DerivedGateCapabilities).
//
// Stages that carry NO requirement and never appear here: an agentic gate
// without runsOn (it evaluates in the daemon/control plane, byte-identical to
// before the field existed — decision 001 ruling 8's "unpinned gate"), every
// automated and human gate (control-plane by definition, ruling 2; runsOn on
// one is WF023), and every parallel. Emitting a row only for a DECLARED gate
// is what keeps ruling 5 true by construction — every placed gate carries
// cpu/memory — and keeps a config that validated clean before this field
// existed validating clean after it.
//
// Consumers key on StageRequirement.Stage (the name), never on position:
// the run-start pin (bootstrap.PinStagePlacements) looks each row up by name
// against the task and gate lists, and the validate/boot checkpoints report
// by name.
func StagePlacements(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn, goobers map[string]apiv1.GooberSpec) []runnersolve.StageRequirement {
	stages := placeableStages(def, goobers)
	requirements := make([]runnersolve.StageRequirement, 0, len(stages))
	for _, stage := range stages {
		effective := EffectiveRunsOn(stage, gaggleRunsOn)
		requirements = append(requirements, runnersolve.StageRequirement{
			Stage:        stage.Name,
			OS:           effective.OS,
			CPU:          effective.CPU,
			Memory:       effective.Memory,
			Disk:         effective.Disk,
			Capabilities: EffectiveCapabilities(stage, gaggleRunsOn),
			Restrictions: effective.Restrictions,
		})
	}
	return requirements
}

// placeableStages is the ordered list StagePlacements walks: tasks, then the
// agentic gates that declare runsOn.
func placeableStages(def Definition, goobers map[string]apiv1.GooberSpec) []PlacementStage {
	stages := make([]PlacementStage, 0, len(def.Spec.Tasks)+len(def.Spec.Gates))
	for _, task := range def.Spec.Tasks {
		stages = append(stages, taskPlacementStage(task, goobers))
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator != apiv1.EvaluatorAgentic || gate.RunsOn == nil {
			continue
		}
		stages = append(stages, gatePlacementStage(gate, goobers))
	}
	return stages
}
