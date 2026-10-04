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
		ref.Lineage, err = retainedChildEpochLineage(ctx, queue, id, ref)
		if err != nil {
			return childExecutionRef{}, err
		}
	} else if ref.Child.RunID != id.RunID {
		return childExecutionRef{}, childworkflow.ErrAuthorityUnavailable
	}
	if ref.Lineage != *id.Child || ref.Envelope.WorkflowDigest != id.WorkflowDigest || ref.Envelope.Workflow != id.Workflow || ref.Envelope.ConfigGeneration != id.ConfigGeneration {
		return childExecutionRef{}, childworkflow.ErrAuthorityUnavailable
	}
	if requireCurrent {
		if ref.Child.ActiveRunID() != id.RunID || ref.Child.CancellationRequested || ref.Child.State.Terminal() {
			return childExecutionRef{}, childworkflow.ErrAuthorityChanged
		}
		if err = queue.CheckChildParentOpen(ctx, identity.ChildParent); err != nil {
			return childExecutionRef{}, err
		}
	}
	return ref, nil
}
func retainedChildEpochLineage(ctx context.Context, queue *triggerqueue.Store, id journal.RunIdentity, ref childExecutionRef) (journal.ChildLineage, error) {
	epoch, err := queue.ChildExecution(ctx, ref.Child.Identity, id.RunID)
	if err != nil {
		return journal.ChildLineage{}, err
	}
	if epoch.Epoch != id.Child.ExecutionEpoch || epoch.SourceRunID != id.ContinuedFromRunID || epoch.SourceTerminalSeq != id.SourceTerminalSeq || epoch.Actor != id.Operator || epoch.Stage != id.RequestedTarget {
		return journal.ChildLineage{}, childworkflow.ErrAuthorityUnavailable
	}
	lineage := ref.Lineage
	lineage.ExecutionEpoch = epoch.Epoch
	lineage.PriorResultRef = epoch.SourceResultRef
	lineage.RestartDigest = epoch.RequestDigest
	return lineage, nil
}
