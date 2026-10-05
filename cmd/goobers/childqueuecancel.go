package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type childQueueCanceller interface {
	CancelControlledStart(context.Context, childExecutionRef, triggerqueue.StartControl) (startcontrol.CancellationObservation, error)
}

func (s *durableTriggerService) cancelChildQueueStart(ctx context.Context, c triggerqueue.StartControl) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	ref, err := s.childReference(ctx, c.Record)
	if err != nil {
		return requested, err
	}
	if c.Cancellation == nil || c.Scope.Gaggle != ref.Envelope.Gaggle || c.Scope.Workflow != ref.Envelope.Workflow || c.Scope.Generation != ref.Envelope.ConfigGeneration || c.Scope.ReservedRunID != ref.Child.RunID {
		return requested, triggerqueue.ErrConflict
	}
	original, err := s.queue.ChildExecutionMetadata(ctx, ref.Child.Identity, c.Scope.ReservedRunID)
	if err != nil {
		return requested, err
	}
	if original.Epoch != 0 || original.RunID != ref.Child.RunID {
		return requested, triggerqueue.ErrConflict
	}
	observer, ok := s.children.(childQueueCanceller)
	if !ok {
		return requested, nil
	}
	return observer.CancelControlledStart(ctx, ref, c)
}

// CancelControlledStart runs under current human policy. It never enters Result,
// acquires execution authority, resumes a runner, or captures a current result.
func (l *queuedChildLauncher) CancelControlledStart(ctx context.Context, ref childExecutionRef, c triggerqueue.StartControl) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	observed, err := acceptedChildObserver(l.layout)(ctx, ref)
	if err != nil || !observed {
		return requested, err
	}
	before, err := l.observeChildQueueCancellation(ctx, ref, c)
	if err != nil || before.State != startcontrol.CancellationRequested {
		return before, err
	}
	// An old queue receipt cannot signal a later human epoch. The initial RunID
	// stays immutable, but even signalling it is unnecessary once superseded.
	if ref.Child.ExecutionEpoch != 0 {
		return requested, nil
	}
	if err = l.Cancel(ctx, ref); err != nil {
		return requested, err
	}
	return l.observeChildQueueCancellation(ctx, ref, c)
}

func (l *queuedChildLauncher) observeChildQueueCancellation(ctx context.Context, ref childExecutionRef, c triggerqueue.StartControl) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	release, owned := l.runners.acquireChildCustody(ref.runID())
	if !owned {
		return requested, nil
	}
	defer release()
	dir, err := acceptedTriggerJournalDir(ctx, l.layout, ref.runID())
	if err != nil || dir == "" {
		return requested, err
	}
	info, err := os.Stat(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return requested, err
	}
	if info.Size() > 32<<20 {
		return requested, errors.New("child cancellation history exceeds bound")
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return requested, err
	}
	// A released workflow owner is insufficient while a contained writer exists.
	// Production reconciliation verifies exact signed attempts and joins custody.
	if l.reconcile == nil {
		return requested, nil
	}
	if err = l.reconcile(ctx, reader); err != nil {
		return requested, err
	}
	writer, report, err := journal.TryRecover(dir)
	if errors.Is(err, journal.ErrRecoveryBusy) {
		return requested, nil
	}
	if err != nil {
		return requested, err
	}
	defer func() { _ = writer.Close() }()
	if observed, err := acceptedChildObserver(l.layout)(ctx, ref); err != nil || !observed {
		return requested, err
	}
	if ref.Child.ExecutionEpoch > 0 && journal.PhaseFromEvents(report.Events) == journal.PhaseEscalated {
		return l.observeSealedInitialChild(ctx, ref, reader, report.Events)
	}
	return controlledTerminalEvents(report.Events, c)
}

func (l *queuedChildLauncher) observeSealedInitialChild(ctx context.Context, ref childExecutionRef, reader *journal.Reader, events []journal.Event) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	id, err := reader.Identity()
	if err != nil {
		return requested, err
	}
	admission, err := runner.PinnedChildWorkspaceAdmission(reader, id)
	if err != nil {
		return requested, err
	}
	var repoURL string
	if admission != nil {
		if id.WorkspaceRepository == nil {
			return requested, triggerqueue.ErrConflict
		}
		repoURL, err = childRepoCloneURL(*id.WorkspaceRepository)
		if err != nil {
			return requested, err
		}
	}
	result, err := (&childworkflow.WorkspaceCoordinator{Queue: l.queue}).ReadExecutionResult(ctx, ref.Child, ref.Child.RunID, repoURL)
	if err != nil {
		return requested, err
	}
	if result.Input.State != triggerqueue.ChildAwaitingHuman {
		return requested, triggerqueue.ErrConflict
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventRunFinished {
			continue
		}
		if event.Status != string(journal.PhaseEscalated) || event.Seq == 0 || event.Time.IsZero() || !event.Time.Equal(result.Input.FinishedAt) {
			return requested, triggerqueue.ErrConflict
		}
		return startcontrol.CancellationObservation{State: startcontrol.CancellationAlreadyTerminal}, nil
	}
	return requested, nil
}
