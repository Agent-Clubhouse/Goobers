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

// StagePlacements returns every task's effective placement requirement, in
// task order. goobers supplies the referenced goober specs for harness
// derivation (see DerivedCapabilities). Gates and parallels carry no
// placement requirement in v1: they execute in the daemon/control plane, not
// on a placed runner (matching the pre-3.0 admission model, which also read
// tasks only).
func StagePlacements(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn, goobers map[string]apiv1.GooberSpec) []runnersolve.StageRequirement {
	requirements := make([]runnersolve.StageRequirement, 0, len(def.Spec.Tasks))
	for _, task := range def.Spec.Tasks {
		effective := EffectiveRunsOn(task, gaggleRunsOn)
		requirements = append(requirements, runnersolve.StageRequirement{
			Stage:        task.Name,
			OS:           effective.OS,
			CPU:          effective.CPU,
			Memory:       effective.Memory,
			Disk:         effective.Disk,
			Capabilities: EffectiveCapabilities(task, gaggleRunsOn, goobers),
			Restrictions: effective.Restrictions,
		})
	}
	return requirements
}
