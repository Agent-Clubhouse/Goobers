package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type childStartObserver func(context.Context, childExecutionRef) (bool, error)

// acceptedChildObserver recognizes exact immutable child provenance. Corrupt,
// ambiguous, pruned or mismatched journals never authorize dispatch replay.
func acceptedChildObserver(layout instance.Layout) childStartObserver {
	return func(ctx context.Context, ref childExecutionRef) (bool, error) {
		dir, err := acceptedTriggerJournalDir(ctx, layout, ref.Child.RunID)
		if err != nil {
			return false, err
		}
		if dir == "" {
			return false, nil
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			return false, err
		}
		id, err := reader.Identity()
		if err != nil {
			return false, err
		}
		e := ref.Envelope
		if id.Child == nil || *id.Child != ref.Lineage || id.RunID != ref.Child.RunID || id.Gaggle != e.Gaggle || id.Workflow != e.Workflow || id.WorkflowDigest != e.WorkflowDigest || id.ConfigGeneration != e.ConfigGeneration {
			return false, fmt.Errorf("accepted child %s has a mismatched run identity", ref.Child.AcceptanceID)
		}
		return true, nil
	}
}

func (s *durableTriggerService) reconcileChildReceipt(ctx context.Context, record triggerqueue.Record) error {
	if s.observeChild == nil {
		return nil
	}
	ref, err := s.childReference(ctx, record)
	if err != nil {
		return err
	}
	observed, err := s.observeChild(ctx, ref)
	if err != nil {
		return err
	}
	if !observed && s.bootUncertain[record.ID] {
		if err = s.queue.RequeueChild(ctx, ref.Child.Identity, "no execution observed after restart", s.dispatch.now()); err != nil {
			return err
		}
		delete(s.bootUncertain, record.ID)
		return nil
	}
	if err = s.reconcileObservation(ctx, record, observed); err != nil {
		return err
	}
	if observed {
		return s.recordChildRunning(ctx, ref.Child)
	}
	return nil
}

func (s *durableTriggerService) recordChildRunning(ctx context.Context, child triggerqueue.ChildRecord) error {
	if child.State != triggerqueue.ChildQueued {
		return nil
	}
	return s.queue.SetChildState(ctx, child.Identity, triggerqueue.ChildStateUpdate{Expected: child.State, State: triggerqueue.ChildRunning}, s.dispatch.now())
}

// Unknown worker observations share one pass budget. The cursor is advanced
// before each attempt so a slow child cannot starve later families on retry.
const childReconcilePassBudget = 10 * time.Second

// This bounded sweep also visits Dispatched starts: execution reconciliation
// must outlive start acceptance. Parent fences are a durable cancellation outbox;
// cancellation is retried until a verified terminal result removes the entry.
func (s *durableTriggerService) reconcileChildren(ctx context.Context) error {
	if s.children == nil || s.observeChild == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, childReconcilePassBudget)
	defer cancel()
	children, err := s.queue.ActiveChildren(ctx, s.childCursor, 100)
	if err != nil {
		return err
	}
	if len(children) == 0 {
		s.childCursor = ""
		return nil
	}
	var failures error
	for _, child := range children {
		if err = ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		s.childCursor = child.ChildID
		failures = errors.Join(failures, s.reconcileChildExecution(ctx, child))
	}
	return failures
}

func (s *durableTriggerService) reconcileChildExecution(ctx context.Context, child triggerqueue.ChildRecord) error {
	record, err := s.queue.ChildStart(ctx, child.Identity)
	if err != nil {
		return err
	}
	ref, err := s.childReference(ctx, record)
	if err != nil {
		return err
	}
	var cancelErr error
	if ref.Child.CancellationRequested {
		cancelErr = s.children.Cancel(ctx, ref)
	}
	// A failed cancel delivery must not hide independently observed completion.
	return errors.Join(cancelErr, s.observeChildExecution(ctx, record, ref))
}

func (s *durableTriggerService) observeChildExecution(ctx context.Context, record triggerqueue.Record, ref childExecutionRef) error {
	if record.State == triggerqueue.Accepted {
		return nil
	}
	observed, err := s.observeChild(ctx, ref)
	if err != nil {
		return err
	}
	if !observed && record.State != triggerqueue.Rejected {
		return nil
	}
	if observed {
		if record.State == triggerqueue.Rejected {
			return fmt.Errorf("rejected child %s has an execution journal", record.ID)
		}
		if record.State == triggerqueue.Dispatching {
			if err = s.reconcileObservation(ctx, record, true); err != nil {
				return err
			}
		}
		if err = s.recordChildRunning(ctx, ref.Child); err != nil {
			return err
		}
		if ref.Child.State == triggerqueue.ChildQueued {
			ref.Child.State = triggerqueue.ChildRunning
		}
	}
	result, err := s.children.Result(ctx, ref)
	if err != nil {
		return err
	}
	return s.recordChildResult(ctx, ref, result, observed)
}

func (s *durableTriggerService) recordChildResult(ctx context.Context, ref childExecutionRef, result childExecutionResult, started bool) error {
	if result.State == "" {
		return nil
	}
	if !started && result.State != triggerqueue.ChildCancelled && result.State != triggerqueue.ChildFailed {
		return errors.New("unstarted child has an invalid result")
	}
	// An unchanged nonterminal observation is not a lifecycle transition. Never
	// erase existing workspace custody when a poll contains no new outcome.
	if result.State == ref.Child.State && !result.State.Terminal() {
		return nil
	}
	return s.queue.SetChildState(ctx, ref.Child.Identity, triggerqueue.ChildStateUpdate{Expected: ref.Child.State, State: result.State, ResultRef: result.ResultRef, WorkspaceRef: result.WorkspaceRef}, s.dispatch.now())
}
