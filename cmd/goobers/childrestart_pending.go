package main

import (
	"context"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// An accepted human epoch is independent of the already-dispatched original
// start receipt. Absence of its journal means queued admission, not permission
// to relaunch the original workflow or capture its prior terminal result.
func (l *queuedChildLauncher) pendingRestartResult(ctx context.Context, ref childExecutionRef) (childExecutionResult, error) {
	if ref.Child.CancellationRequested {
		custody := childworkflow.WorkspaceCoordinator{Queue: l.queue}
		result, err := custody.CaptureResult(ctx, ref.Child, nil, childworkflow.TerminalResultInput{State: triggerqueue.ChildCancelled, FinishedAt: ref.Child.UpdatedAt, Summary: "Human child restart cancelled before execution"})
		return childExecutionResult{State: triggerqueue.ChildCancelled, ResultRef: result.ResultRef}, err
	}
	if l.restart == nil {
		return childExecutionResult{}, nil
	}
	return childExecutionResult{}, l.restart(ctx, ref)
}
