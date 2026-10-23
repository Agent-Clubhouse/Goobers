package main

import (
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/workflow"
)

type localPlacement struct {
	containedParentStages  map[string]bool
	parentHostCapabilities []string
	refusal                string
}

func localPlacements(
	cfg *instance.Config,
	set *instance.ConfigSet,
	machines map[localscheduler.WorkflowIdentity]*workflow.Machine,
) map[localscheduler.WorkflowIdentity]localPlacement {
	placements := make(map[localscheduler.WorkflowIdentity]localPlacement, len(machines))
	for identity, machine := range machines {
		if machine == nil || !hasChildParent(machine.Def.Spec) {
			continue
		}
		placement, err := containedParentSelection(cfg, set, machine)
		if err != nil {
			placement.refusal = err.Error()
		}
		placements[identity] = placement
	}
	return placements
}

// Parent coordination runs the workflow driver locally while the existing
// contained factory owns each opted-in agent stage. Only stages whose exact
// placement was validated here are excluded from the daemon-host solve.
func containedParentSelection(cfg *instance.Config, set *instance.ConfigSet, machine *workflow.Machine) (localPlacement, error) {
	pins, err := containedParentPlacements(cfg, set, machine)
	if err != nil {
		return localPlacement{}, err
	}
	if trustedLocalChildren(cfg) {
		return localPlacement{}, nil
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
		hasLocalStage = hasLocalStage || gate.Evaluator == apiv1.EvaluatorAgentic
	}
	if hasLocalStage {
		for _, gaggle := range set.Gaggles {
			if gaggle.Name == machine.Def.Spec.Gaggle {
				hostCapabilities = instance.WorkflowRequiredCapabilities(gaggle, local)
				break
			}
		}
	}
	return localPlacement{
		containedParentStages:  stages,
		parentHostCapabilities: hostCapabilities,
	}, nil
}

func (selection localPlacement) hostStageRequirements(requirements []runnersolve.StageRequirement) []runnersolve.StageRequirement {
	if len(selection.containedParentStages) == 0 {
		return requirements
	}
	return slices.DeleteFunc(slices.Clone(requirements), func(stage runnersolve.StageRequirement) bool {
		return selection.containedParentStages[stage.Stage]
	})
}

// The ordinary tracked starter keeps its complete capability floor; a contained
// coordinator needs only the capabilities of stages that still execute locally.
func (selection localPlacement) parentCoordinatorHostCapabilities(required []string) []string {
	if len(selection.containedParentStages) == 0 {
		return required
	}
	return append([]string(nil), selection.parentHostCapabilities...)
}
