package runner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// ContainedParentWorkspaceKind records the managed checkout held before its
// isolated worker can start. The pod never receives this host custody marker.
const ContainedParentWorkspaceKind = "isolated.parent.workspace"

// ContainedParentWorkspaceCustody is a host-only recovery receipt. It contains
// no filesystem path, clone URL, credentials or permission grant.
type ContainedParentWorkspaceCustody struct {
	Version   int                        `json:"version"`
	Origin    *apiv1.ChildWorkflowOrigin `json:"origin"`
	Workspace worktree.StageCustody      `json:"workspace"`
}

func (r *Runner) holdContainedParentWorkspace(ctx context.Context, tf taskFrame, workspace *stageWorkspace, env apiv1.InvocationEnvelope) (func() error, error) {
	if r.cfg.ChildWorkflowAdmission == nil {
		return func() error { return nil }, nil
	}
	custody, err := workspace.worktree.HoldForChild(ctx)
	if err != nil {
		return nil, err
	}
	// On any publication error preserve custody rather than reaping the original
	// parent work. A normal joined invocation releases this hold below.
	preserve := func(context.Context) error { return nil }
	workspace.retainedChild = preserve
	data, err := json.Marshal(ContainedParentWorkspaceCustody{Version: 1, Origin: env.ChildWorkflowOrigin, Workspace: custody})
	if err != nil {
		return nil, err
	}
	var record map[string]any
	if err = json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if err = tf.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: tf.t.Name, Attempt: int(env.Attempt), Runner: map[string]any{"kind": ContainedParentWorkspaceKind, "custody": record}}); err != nil {
		return nil, err
	}
	workspace.retainedChild = nil
	if tf.heldChildWorkspace == workspace {
		workspace.retainedChild = preserve
	}
	return func() error {
		if workspace.retainedChild != nil {
			return nil
		} // transferred to child wait
		if err := r.verifyChildWorkflowCustody(tf.jr.Dir()); err != nil {
			workspace.retainedChild = preserve
			return err
		}
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := workspace.worktree.ReleaseChildHold(releaseCtx); err != nil {
			workspace.retainedChild = preserve
			return errors.Join(err, ctx.Err())
		}
		return nil
	}, nil
}
