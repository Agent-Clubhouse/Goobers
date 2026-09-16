package workerhost

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/internal/worktree"
)

// Validate configured ownership before provisioning, then verify the checkout
// before exposing it. A failed verification must also dispose of the workspace.
func (p *WorktreeWorkspaces) provisionOwnedWorkspace(ctx context.Context, req engine.WorkspaceRequest) (engine.Workspace, error) {
	binding := req.WorkspaceBranchBinding
	if _, err := workspacebranch.Accept(nil, binding, req.WorkspaceRevision, p.ConfiguredBase,
		req.BranchNamespace, req.Workflow, req.RunID, true, true); err != nil {
		return nil, err
	}
	if req.SyncBase || "refs/heads/"+req.WorkspaceBranch != binding.Ref {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeConflict, Message: "owned workspace cannot change branch or synchronize base"}
	}
	req.RepoRef = p.ConfiguredBase
	workspace, err := p.provisionWorkspace(ctx, req)
	if err != nil || workspace == nil {
		return workspace, err
	}
	if err := worktree.VerifyOwnedWorkspace(ctx, workspace.Path(), *binding); err != nil {
		return nil, errors.Join(err, workspace.Remove(ctx))
	}
	return workspace, nil
}
