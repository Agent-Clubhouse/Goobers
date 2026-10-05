package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// retainedChildExecutionRef verifies immutable accepted provenance for either
// the original run or a bounded human epoch. Historical lookup never permits
// new effects; requireCurrent additionally fences the active execution and the
// parent. Credential callers must still verify current policy and stage lease.
func retainedChildExecutionRef(ctx context.Context, queue *triggerqueue.Store, id journal.RunIdentity, requireCurrent bool) (childExecutionRef, error) {
	if queue == nil || id.Child == nil || id.ValidateChildLineage() != nil {
		return childExecutionRef{}, childworkflow.ErrAuthorityUnavailable
	}
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: id.Gaggle, ParentRunID: id.Child.ParentRunID}, StageOccurrence: id.Child.StageOccurrence, InvocationKey: id.Child.InvocationKey}
	receipt, err := queue.ChildStart(ctx, identity)
	if err != nil {
		return childExecutionRef{}, err
	}
	if receipt.State != triggerqueue.Dispatching && receipt.State != triggerqueue.Dispatched {
		return childExecutionRef{}, errors.New("child execution has no claimed start")
	}
	service := durableTriggerService{queue: queue}
	ref, err := service.childReference(ctx, receipt)
	if err != nil {
		return childExecutionRef{}, err
	}
	if id.Child.ExecutionEpoch > 0 {
		ref, err = childEpochReference(ctx, queue, ref, id.RunID)
		if err != nil {
			return childExecutionRef{}, err
		}
		if ref.Execution.SourceRunID != id.ContinuedFromRunID || ref.Execution.SourceTerminalSeq != id.SourceTerminalSeq || ref.Execution.Actor != id.Operator || ref.Execution.Stage != id.RequestedTarget {
			return childExecutionRef{}, childworkflow.ErrAuthorityUnavailable
		}
	} else if ref.Child.RunID != id.RunID {
		return childExecutionRef{}, childworkflow.ErrAuthorityUnavailable
	}
	if ref.Lineage != *id.Child || ref.Envelope.WorkflowDigest != id.WorkflowDigest || ref.Envelope.Workflow != id.Workflow || ref.Envelope.ConfigGeneration != id.ConfigGeneration {
		return childExecutionRef{}, childworkflow.ErrAuthorityUnavailable
	}
	if requireCurrent {
		if err = requireCurrentChildExecution(ctx, queue, ref, id.RunID); err != nil {
			return childExecutionRef{}, err
		}
	}
	return ref, nil
}
func childEpochReference(ctx context.Context, queue *triggerqueue.Store, ref childExecutionRef, runID string) (childExecutionRef, error) {
	epoch, err := queue.ChildExecution(ctx, ref.Child.Identity, runID)
	if err != nil {
		return childExecutionRef{}, err
	}
	if epoch.Epoch < 1 {
		return childExecutionRef{}, childworkflow.ErrAuthorityUnavailable
	}
	ref.Execution = &epoch
	ref.Lineage.ExecutionEpoch = epoch.Epoch
	ref.Lineage.PriorResultRef = epoch.SourceResultRef
	ref.Lineage.RestartDigest = epoch.RequestDigest
	return ref, nil
}

func (ref childExecutionRef) runID() string {
	if ref.Execution != nil {
		return ref.Execution.RunID
	}
	return ref.Child.RunID
}

func (ref childExecutionRef) executionIdentity(gooberDigest string) journal.RunIdentity {
	id := journal.RunIdentity{RunID: ref.runID(), Gaggle: ref.Envelope.Gaggle, Workflow: ref.Envelope.Workflow, WorkflowDigest: ref.Envelope.WorkflowDigest, GooberDigest: gooberDigest, ConfigGeneration: ref.Envelope.ConfigGeneration, Child: &ref.Lineage}
	if ref.Execution != nil {
		id.ContinuedFromRunID, id.SourceTerminalSeq, id.Operator, id.RequestedTarget = ref.Execution.SourceRunID, ref.Execution.SourceTerminalSeq, ref.Execution.Actor, ref.Execution.Stage
	}
	return id
}

func requireCurrentChildExecution(ctx context.Context, queue *triggerqueue.Store, ref childExecutionRef, runID string) error {
	if ref.Child.ActiveRunID() != runID || ref.Child.CancellationRequested || ref.Child.State.Terminal() {
		return childworkflow.ErrAuthorityChanged
	}
	if cancelled, err := queue.ChildInitialStartCancelled(ctx, ref.Child); err != nil {
		return err
	} else if cancelled {
		return childworkflow.ErrAuthorityChanged
	}
	return queue.CheckChildParentOpen(ctx, ref.Child.Identity.ChildParent)
}
