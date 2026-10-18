package main

import (
	"fmt"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Cleanup of accepted custody still requires an enabled current parent stage.
// Removing its child opt-in revokes execution, not ownership of prior results.
func childCurrentCustodyPolicy(cfg *instance.Config, source *instance.ConfigSet, parent childworkflow.ParentSelection) (childworkflow.AdmissionContext, error) {
	selected, err := childSelectedConfig(source, parent)
	if err != nil {
		return childworkflow.AdmissionContext{}, err
	}
	instance.ApplyGaggleCICommand(selected)
	instance.ApplyGaggleOutboxMirror(selected)
	admitted, err := admitChildValidationGoobers(cfg, goobersByName(selected))
	if err != nil {
		return childworkflow.AdmissionContext{}, err
	}
	machines, err := compileWorkflowMachines(selected, goobersByName(selected), admitted.HarnessNames, cfg.ExternalTelemetryConnectorNames())
	if err != nil {
		return childworkflow.AdmissionContext{}, err
	}
	machine := machines[localscheduler.WorkflowIdentity{Gaggle: parent.Gaggle, Workflow: parent.Workflow}]
	if machine == nil {
		return childworkflow.AdmissionContext{}, childworkflow.ErrAuthorityUnavailable
	}
	task, ok := machine.Task(parent.Stage)
	if !ok || task.Type != apiv1.TaskAgentic {
		return childworkflow.AdmissionContext{}, fmt.Errorf("current child parent stage is unavailable")
	}
	policy := childworkflow.AdmissionContext{Gaggle: selected.Gaggles[0], ParentTask: task, Goobers: admitted.Goobers, GrantedCapabilities: slices.Clone(task.Capabilities)}
	if task.ChildWorkflows != nil {
		policy.AllowPRPublication = task.ChildWorkflows.AllowPRPublication
	}
	return policy, nil
}
