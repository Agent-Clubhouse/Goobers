package runner

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacebranch"
)

func ownedBranchStartingSHA(binding *apiv1.WorkspaceBranchBinding) string {
	if binding == nil {
		return ""
	}
	return binding.StartingSHA
}

// RestoredWorkspaceBranchBinding reconstructs ownership from the full history,
// including the selected revision that preceded its establishment.
func RestoredWorkspaceBranchBinding(events []journal.Event, machine *workflow.Machine, base apiv1.RepoRef, additional []apiv1.RepoRef, namespace, runID string) (*apiv1.WorkspaceBranchBinding, error) {
	var binding *apiv1.WorkspaceBranchBinding
	in := StartInput{Machine: machine, RepoRef: base}
	for _, event := range events {
		if event.WorkspaceBranchBinding != nil && (event.Type != journal.EventStageFinished || event.Branch != 0) {
			return nil, fmt.Errorf("remote workspace ownership must be recorded on a serial stage result")
		}
		if event.Type != journal.EventStageFinished {
			continue
		}
		task, ok := machine.Task(event.Stage)
		if !ok {
			if event.WorkspaceBranchBinding != nil || event.WorkspaceRevision != nil {
				return nil, fmt.Errorf("workspace control has no pinned producer")
			}
			continue
		}
		result := apiv1.ResultEnvelope{
			Status: apiv1.ResultStatus(event.Status), Outputs: event.Outputs,
			WorkspaceRevision: event.WorkspaceRevision, WorkspaceBranchBinding: event.WorkspaceBranchBinding,
		}
		selected, err := acceptWorkspaceRevision(in, task, result, additional)
		if err != nil {
			return nil, err
		}
		binding, err = workspacebranch.ValidateResult(binding, in.workspaceRevision, base, namespace, machine.Def.Name, runID, task, result)
		if err != nil {
			return nil, fmt.Errorf("restore remote workspace ownership at event %d: %w", event.Seq, err)
		}
		in.workspaceRevision = selected
	}
	return binding, nil
}
