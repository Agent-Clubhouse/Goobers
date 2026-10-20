package runner

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Preserve the simple verifier seam for embedders while allowing the production
// verifier to resolve SDK pins from the same repository the stages will use.
func (r *Runner) verifyToolchains(ctx context.Context, in StartInput) (retErr error) {
	v, ok := r.toolchains.(interface {
		VerifyIn(context.Context, []string, string) error
	})
	if !ok {
		return r.toolchains.Verify(ctx, in.RequiredCapabilities)
	}
	if !machineUsesRepo(in.Machine) {
		return v.VerifyIn(ctx, in.RequiredCapabilities, "")
	}
	if in.pinnedWorkspace != nil {
		return v.VerifyIn(ctx, in.RequiredCapabilities, in.pinnedWorkspace.Path)
	}
	if in.childWorkspace != nil {
		return v.VerifyIn(ctx, in.RequiredCapabilities, in.childWorkspace.worktree.Path)
	}
	// Preflight precedes stage provisioning. Use a detached checkout, released
	// before dispatch, so probing neither creates nor occupies the run branch.
	if in.WorkspaceBranch != "" {
		in.RepoRef.Branch = in.WorkspaceBranch
	}
	if in.WorkspaceBranchSHA != "" {
		in.RepoRef.Branch = in.WorkspaceBranchSHA
	}
	workspace, err := r.createStageWorkspace(ctx, in, toolchainPreflightState, apiv1.WorkspaceRepoReadOnly, false, "")
	if err != nil {
		return fmt.Errorf("toolchain preflight checkout: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, workspace.Remove(context.WithoutCancel(ctx)))
	}()
	return v.VerifyIn(ctx, in.RequiredCapabilities, workspace.path)
}
