package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Caller holds exclusive source registry custody. All physical writers must be
// joined before sealing an escalated source; journal terminal alone is not proof.
func (l *queuedChildLauncher) sealRestartSource(ctx context.Context, plan runner.StageRestartPlan) (childExecutionRef, string, error) {
	ref, err := retainedChildExecutionRef(ctx, l.queue, plan.Source, false)
	if err != nil {
		return ref, "", err
	}
	if ref.Child.ActiveRunID() != plan.Source.RunID {
		return ref, "", triggerqueue.ErrTransition
	}
	if err = l.queue.CheckChildParentOpen(ctx, ref.Child.Identity.ChildParent); err != nil {
		return ref, "", err
	}
	dir, err := l.layout.FindRunDir(plan.Source.RunID)
	if err != nil {
		return ref, "", err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return ref, "", err
	}
	if l.reconcile != nil {
		if err = l.reconcile(ctx, reader); err != nil {
			return ref, "", err
		}
	}
	events, err := reader.Events()
	if err != nil {
		return ref, "", err
	}
	phase := journal.PhaseFromEvents(events)
	if phase != journal.PhaseFailed && phase != journal.PhaseEscalated {
		return ref, "", errors.New("child restart requires sealed failed or escalated source")
	}
	input, found, err := childTerminalInput(reader, events, phase)
	if err != nil || !found {
		return ref, "", errors.Join(err, errors.New("child restart source terminal evidence unavailable"))
	}
	custody := childworkflow.WorkspaceCoordinator{Queue: l.queue}
	workspace, err := l.terminalWorkspace(ctx, &custody, ref, reader, plan.Source)
	if err != nil {
		return ref, "", err
	}
	result, err := custody.CaptureResult(ctx, ref.Child, workspace, input)
	if err != nil {
		return ref, "", err
	}
	if ref.Child.State == triggerqueue.ChildQueued && input.State == triggerqueue.ChildAwaitingHuman {
		if err = l.queue.SetChildState(ctx, ref.Child.Identity, triggerqueue.ChildStateUpdate{Expected: ref.Child.State, State: triggerqueue.ChildRunning, ExecutionRunID: ref.runID()}, l.dispatch.now()); err != nil {
			return ref, "", err
		}
		ref.Child.State = triggerqueue.ChildRunning
	}
	if ref.Child.State != input.State {
		update := triggerqueue.ChildStateUpdate{Expected: ref.Child.State, State: input.State, ExecutionRunID: ref.runID()}
		if input.State.Terminal() {
			update.ResultRef, update.WorkspaceRef = result.ResultRef, result.WorkspaceRef
		}
		if err = l.queue.SetChildState(ctx, ref.Child.Identity, update, l.dispatch.now()); err != nil {
			return ref, "", err
		}
	}
	return ref, result.ResultRef, nil
}
