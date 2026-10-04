package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// resolveGeneration uses the same retained source custody as initial dispatch.
// It never selects a generated workflow by its display name and never provisions
// a replacement workspace. The normal resume path verifies/adopts pinned custody.
func (l *queuedChildLauncher) resolveGeneration(ctx context.Context, id journal.RunIdentity) (executionGenerationRuntime, error) {
	if id.Child == nil || l.build == nil {
		return executionGenerationRuntime{}, childworkflow.ErrAuthorityUnavailable
	}
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: id.Gaggle, ParentRunID: id.Child.ParentRunID}, StageOccurrence: id.Child.StageOccurrence, InvocationKey: id.Child.InvocationKey}
	receipt, err := l.queue.ChildStart(ctx, identity)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	if receipt.State != triggerqueue.Dispatching && receipt.State != triggerqueue.Dispatched {
		return executionGenerationRuntime{}, errors.New("generated recovery has no claimed start")
	}
	service := durableTriggerService{queue: l.queue}
	ref, err := service.childReference(ctx, receipt)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	if ref.Child.RunID != id.RunID || ref.Lineage != *id.Child || ref.Envelope.WorkflowDigest != id.WorkflowDigest || ref.Envelope.Workflow != id.Workflow || ref.Envelope.ConfigGeneration != id.ConfigGeneration {
		return executionGenerationRuntime{}, errors.New("generated recovery identity differs from accepted source")
	}
	if ref.Child.CancellationRequested || ref.Child.State.Terminal() {
		return executionGenerationRuntime{}, triggerqueue.ErrParentCancelled
	}
	a, release, err := l.acquire(ctx, ref.Envelope)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	defer release()
	source, err := l.queue.ChildProposal(ctx, identity)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	proposal, err := childworkflow.ValidateRetainedStart(a, ref.Envelope, source.Source)
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	runtime, err := l.build(ctx, childExecutionStart{childExecutionRef: ref, Proposal: proposal})
	if err != nil {
		return executionGenerationRuntime{}, err
	}
	defer runtime.release()
	if runtime.machine.Digest() != id.WorkflowDigest || runtime.gooberDigest != id.GooberDigest {
		return executionGenerationRuntime{}, errors.New("generated recovery runtime differs from journal pins")
	}
	// The already-retained journal and queue source remain durable generation
	// owners after this short construction lease ends.
	return runtime.executionGenerationRuntime, nil
}

func (r *daemonRunnerRegistry) setChildGenerationResolver(resolve executionGenerationResolver) {
	r.mu.Lock()
	r.resolveChildGeneration = resolve
	r.mu.Unlock()
}
