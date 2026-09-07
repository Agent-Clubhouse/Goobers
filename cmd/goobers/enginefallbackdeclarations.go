package main

import (
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

func workflowDeclaresPlacement(def workflow.Definition) bool {
	for _, task := range def.Spec.Tasks {
		if task.RunsOn != nil {
			return true
		}
	}
	for _, gate := range def.Spec.Gates {
		if gate.RunsOn != nil {
			return true
		}
	}
	return false
}

func entryDeclaresPlacement(machine *workflow.Machine, set *instance.ConfigSet, gaggle string) bool {
	if machine != nil && workflowDeclaresPlacement(machine.Def) {
		return true
	}
	if set != nil {
		for _, value := range set.Gaggles {
			if value.Name == gaggle && value.Spec.RunsOn != nil {
				return true
			}
		}
	}
	return false
}
