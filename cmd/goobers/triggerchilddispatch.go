package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// childExecutionLauncher is intentionally not installed until dynamic capacity,
// parent snapshot custody and workspace adoption are available. Prepare resolves
// the parent's pinned generation intersected with CURRENT execution authority.
// Start must acquire normal capacity and recheck revocation/cancellation under
// its execution lease, publish the exact journal identity before effects, and
// hand off this reserved RunID only once. It must never register a catalog alias.
// Returning an error is ambiguous unless it is childStartDeferred: a transport
// failure after handoff is not evidence that no run was started.
// Cancel is repeatable and never itself proves termination. Result returns only
// durable, verified result/workspace custody after execution actually stops;
// an empty observation means the outcome is not yet available.
type childExecutionLauncher interface {
	Prepare(context.Context, childworkflow.ChildStartEnvelope) (childworkflow.Authority, error)
	Start(context.Context, childExecutionStart) error
	Cancel(context.Context, childExecutionRef) error
	Result(context.Context, childExecutionRef) (childExecutionResult, error)
}

type childExecutionRef struct {
	Child     triggerqueue.ChildRecord
	Envelope  childworkflow.ChildStartEnvelope
	Lineage   journal.ChildLineage
	Execution *triggerqueue.ChildExecution
}

type childExecutionStart struct {
	childExecutionRef
	Proposal *childworkflow.Proposal
}

type childExecutionResult struct {
	State        triggerqueue.ChildState
	ResultRef    string
	WorkspaceRef string
}

// childStartDeferred is a trusted adapter's explicit proof that it has neither
// started nor handed off execution. Only temporary admission/capacity refusal
// should use it; unknown outcomes retain the dispatch claim for reconciliation.
type childStartDeferred struct{ Reason string }

func (e *childStartDeferred) Error() string { return e.Reason }

func (s *durableTriggerService) childReference(ctx context.Context, record triggerqueue.Record) (childExecutionRef, error) {
	e, err := childworkflow.DecodeStartEnvelope(record.Payload)
	if err != nil {
		return childExecutionRef{}, err
	}
	c, err := s.queue.GetChild(ctx, e.Identity())
	if err != nil {
		return childExecutionRef{}, err
	}
	if c.AcceptanceID != record.ID || c.StartKey != record.Key || c.ProposalDigest != e.SourceDigest || c.RunID == "" || (record.RunID != "" && c.RunID != record.RunID) || !c.TombstonedAt.IsZero() {
		return childExecutionRef{}, childworkflow.ErrSubmissionInvalid
	}
	lineage := journal.ChildLineage{Gaggle: e.Gaggle, ParentRunID: e.ParentRunID, ParentWorkflow: e.ParentWorkflow, StageOccurrence: e.StageOccurrence, InvocationKey: e.InvocationKey, AcceptanceID: record.ID, SourceDigest: e.SourceDigest, EnvelopeDigest: journal.Digest(record.Payload)}
	id := journal.RunIdentity{RunID: c.RunID, Gaggle: e.Gaggle, Child: &lineage, ConfigGeneration: e.ConfigGeneration, WorkflowDigest: e.WorkflowDigest, GooberDigest: e.ParentGooberDigest}
	if err := id.ValidateChildLineage(); err != nil {
		return childExecutionRef{}, err
	}
	return childExecutionRef{Child: c, Envelope: e, Lineage: lineage}, nil
}

func (s *durableTriggerService) drainChild(ctx context.Context, record triggerqueue.Record) error {
	if s.children == nil {
		return nil
	}
	ref, err := s.childReference(ctx, record)
	if err != nil {
		return err
	}
	if ref.Child.CancellationRequested || ref.Child.State != triggerqueue.ChildQueued {
		return nil
	}
	authority, err := s.children.Prepare(ctx, ref.Envelope)
	if err != nil {
		return err
	}
	if authority.Actor != record.Actor {
		return childworkflow.ErrAuthorityUnavailable
	}
	source, err := s.queue.ChildProposal(ctx, ref.Child.Identity)
	if err != nil {
		return err
	}
	proposal, err := childworkflow.ValidateRetainedStart(authority, ref.Envelope, source.Source)
	if err != nil {
		return err
	}
	if err = s.auditChildDispatch(record, ref); err != nil {
		return err
	}
	if err = s.queue.BeginDispatch(ctx, record.ID); err != nil {
		if errors.Is(err, triggerqueue.ErrTransition) {
			return nil
		}
		return err
	}
	err = s.children.Start(ctx, childExecutionStart{childExecutionRef: ref, Proposal: proposal})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		var deferred *childStartDeferred
		if errors.As(err, &deferred) {
			return s.queue.RequeueChild(ctx, ref.Child.Identity, deferred.Reason, s.dispatch.now())
		}
		// Keep uncertain custody even for an unexpected local error: only observed
		// durable evidence, or strong startup absence, can resolve a claimed start.
		return fmt.Errorf("child dispatch %s is uncertain: %w", record.ID, err)
	}
	return s.queue.RecordDispatch(ctx, record.ID, ref.Child.RunID)
}

func (s *durableTriggerService) auditChildDispatch(record triggerqueue.Record, ref childExecutionRef) error {
	if s.auditLog == nil {
		return nil
	}
	return s.auditLog.Append(journal.Event{Type: journal.EventRunnerAnnotation, Actor: record.Actor, Workflow: ref.Envelope.Workflow, Gaggle: ref.Envelope.Gaggle, RunID: ref.Child.RunID, Reason: "accepted child dispatch requested", Runner: map[string]any{
		"note": "child.dispatch.requested", "acceptanceId": record.ID, "parentRunId": ref.Envelope.ParentRunID, "stageOccurrence": ref.Envelope.StageOccurrence, "sourceDigest": ref.Envelope.SourceDigest,
	}})
}
