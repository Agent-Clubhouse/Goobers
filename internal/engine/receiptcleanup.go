package engine

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mutationsidecar"
)

// removeWorkspaceWithReceipts runs before the activity's result can be durably
// acknowledged by Temporal. Preserve receipts independently of that result;
// an error or worker interruption must not leave a performed merge unrecorded.
func (a *Activities) removeWorkspaceWithReceipts(ctx context.Context, env apiv1.InvocationEnvelope, ws Workspace) {
	if ws == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceTeardownTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- mutationsidecar.PublishBeforeCleanup(cleanupCtx, ws.Path(), env.TaskID, env.RunID, env.Gaggle, a.scrubber(), a.Journal)
	}()
	var err error
	select {
	case err = <-done:
	case <-cleanupCtx.Done():
		err = fmt.Errorf("mutation receipt handoff timed out: %w", cleanupCtx.Err())
	}
	if err != nil {
		reportWorkspaceTeardownFailure(ctx, env.TaskID, ws.Path(), err)
		return
	}
	removeWorkspace(ctx, env.TaskID, ws)
}
