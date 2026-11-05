package runner

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/workspaceauthority"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// ContainedParentWorkspaceKind uses the shared workspaceauthority.ContainedParentWorkspaceKind contract.
const ContainedParentWorkspaceKind = workspaceauthority.ContainedParentWorkspaceKind

// ContainedParentWorkspaceCustody uses the shared workspaceauthority.ContainedParentWorkspaceCustody contract.
type ContainedParentWorkspaceCustody = workspaceauthority.ContainedParentWorkspaceCustody

// The host must retain the checkout before an isolated invocation can be
// submitted. A transport error is not evidence that its writer has stopped.
func holdContainedParentWorkspace(ctx context.Context, tf taskFrame, workspace *stageWorkspace, env apiv1.InvocationEnvelope) error {
	if workspace == nil || workspace.worktree == nil || env.ChildWorkflowOrigin == nil || tf.childOrigin == nil || *env.ChildWorkflowOrigin != *tf.childOrigin || env.RunID != tf.in.RunID {
		return errors.New("parent invocation lacks exact managed workspace custody")
	}
	custody, err := workspace.worktree.HoldForChild(ctx)
	if err != nil {
		return err
	}
	// A failed journal append still leaves the original checkout protected.
	// Retirement requires verified output and independent writer acknowledgement.
	workspace.retainedChild = func(context.Context) error { return nil }
	workspace.parentContribution = true
	origin := *env.ChildWorkflowOrigin
	return tf.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: tf.t.Name, Attempt: int(env.Attempt), Runner: map[string]any{
		"kind":    ContainedParentWorkspaceKind,
		"custody": ContainedParentWorkspaceCustody{Version: 1, Origin: &origin, Workspace: custody},
	}})
}
