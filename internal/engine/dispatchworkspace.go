package engine

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func stampDispatchWorkspaceAuthority(attempt *dispatcher.Attempt, input DispatchStageInput, workspace apiv1.WorkspaceMode) error {
	attempt.WorkspaceBranchBinding = input.Envelope.WorkspaceBranchBinding.DeepCopy()
	attempt.BranchNamespace = input.Envelope.BranchNamespace
	if attempt.WorkspaceBranchBinding != nil {
		attempt.WorkspaceRepository = input.Envelope.RepoRef
		attempt.Checkout = input.Checkout
		attempt.PartialClone = input.PartialClone
	}
	if workspace == apiv1.WorkspaceRepoReadOnly {
		if err := stampDispatchReadOnlyRevision(attempt, input); err != nil {
			return err
		}
	}
	// Legacy checkout credentials remain separate from stage capabilities.
	// Typed authority instead uses the journal-authorized credential plane.
	if workspace.IsRepoBacked() && attempt.WorkspaceRevision == nil && attempt.WorkspaceBranchBinding == nil && !declaresRepoCapability(attempt.Capabilities) {
		attempt.CheckoutCapability = string(capability.RepoPush)
	}
	return nil
}

func stampDispatchReadOnlyRevision(attempt *dispatcher.Attempt, input DispatchStageInput) error {
	selected, err := workspacerevision.Accept(input.Envelope.WorkspaceRevision, input.WorkspaceRevision, true, true)
	if err != nil {
		return classifySeamError(err)
	}
	attempt.WorkspaceRevision = selected.DeepCopy()
	attempt.WorkspaceRepository = input.Envelope.RepoRef
	attempt.PartialClone = input.PartialClone
	if input.Checkout != nil {
		policy := *input.Checkout
		policy.Sparse = append([]string(nil), policy.Sparse...)
		attempt.Checkout = &policy
	}
	if selected != nil && (input.WorkspaceBranch != "" || input.WorkspaceDelta != "" || (input.Run != nil && input.Run.SyncBase)) {
		return classifySeamError(&workspacerevision.Error{Code: workspacerevision.CodeConflict,
			Message: "selected-revision repo-readonly cannot use branch, delta or syncBase"})
	}
	return nil
}
