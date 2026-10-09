package main

import (
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/workflow"
)

// Parent coordination runs the workflow driver locally while the existing
// contained factory owns each opted-in agent stage. Only stages whose exact
// placement was validated here are excluded from the daemon-host solve.
func containedParentSelection(cfg *instance.Config, set *instance.ConfigSet, machine *workflow.Machine) (engineSelection, error) {
	pins, err := containedParentPlacements(cfg, set, machine)
	if err != nil {
		return engineSelection{}, err
	}
	stages := make(map[string]bool, len(pins))
	for _, pin := range pins {
		stages[pin.Stage] = true
	}
	local := apiv1.Workflow{Spec: machine.Def.Spec}
	local.Spec.Tasks = slices.DeleteFunc(slices.Clone(local.Spec.Tasks), func(task apiv1.Task) bool { return stages[task.Name] })
	var hostCapabilities []string
	// A gaggle's placement floor applies to execution stages, not to a driver
	// that only coordinates remote work. Ordinary stages retain that floor.
	hasLocalStage := len(local.Spec.Tasks) != 0
	for _, gate := range local.Spec.Gates {
		hasLocalStage = hasLocalStage || agenticGate(gate)
	}
	if hasLocalStage {
		for _, gaggle := range set.Gaggles {
			if gaggle.Name == machine.Def.Spec.Gaggle {
				hostCapabilities = instance.WorkflowRequiredCapabilities(gaggle, local)
				break
			}
		}
	}
	return engineSelection{
		ReasonClass:            "contained_parent_coordinator",
		FallbackReason:         "the daemon coordinates child-enabled stages through their pinned contained workers",
		ContainedParentStages:  stages,
		ParentHostCapabilities: hostCapabilities,
	}, nil
}

func (selection engineSelection) hostStageRequirements(requirements []runnersolve.StageRequirement) []runnersolve.StageRequirement {
	if len(selection.ContainedParentStages) == 0 {
		return requirements
	}
	return slices.DeleteFunc(slices.Clone(requirements), func(stage runnersolve.StageRequirement) bool {
		return selection.ContainedParentStages[stage.Stage]
	})
}
