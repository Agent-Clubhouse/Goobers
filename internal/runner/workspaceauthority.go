package runner

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func (r *Runner) validateTaskWorkspaceResult(in StartInput, task apiv1.Task, branch int, result apiv1.ResultEnvelope) (apiv1.ResultEnvelope, error) {
	if result.WorkspaceRevision != nil {
		if branch != 0 && in.workspaceRevision == nil {
			return apiv1.ResultEnvelope{}, codedStageFailure(workspacerevision.CodeConflict, fmt.Errorf("workspace revision must be established before parallel execution"))
		}
		if _, err := r.acceptWorkspaceRevision(in, task, result); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		if result.Status != apiv1.ResultSuccess {
			result.WorkspaceRevision = nil
		}
	}
	if branch != 0 && result.WorkspaceBranchBinding != nil {
		return apiv1.ResultEnvelope{}, codedStageFailure(workspacerevision.CodeConflict, fmt.Errorf("remote branch establishment is not allowed in parallel branches"))
	}
	if _, err := workspacebranch.ValidateResult(in.workspaceBranchBinding, in.workspaceRevision, in.RepoRef,
		r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID, task, result); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if result.Status != apiv1.ResultSuccess {
		result.WorkspaceBranchBinding = nil
		result.WorkspaceBranchTip = ""
	}
	return result, nil
}

func (r *Runner) applyTaskWorkspaceResult(ws *walkState, task apiv1.Task, result apiv1.ResultEnvelope) error {
	if result.WorkspaceRevision != nil {
		selected, err := r.acceptWorkspaceRevision(ws.in, task, result)
		if err != nil {
			return err
		}
		ws.in.workspaceRevision = selected
	}
	if result.WorkspaceBranchBinding != nil {
		ws.in.workspaceBranchBinding = result.WorkspaceBranchBinding.DeepCopy()
		ws.workspaceBranch = strings.TrimPrefix(result.WorkspaceBranchBinding.Ref, "refs/heads/")
	}
	return nil
}
