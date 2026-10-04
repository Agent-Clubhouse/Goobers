package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Current human authority is held by startcontrol.Service for this whole call.
// No queue request is evidence that a worker stopped or that a launch is safe.
func (u *upSession) cancelControlledStart(ctx context.Context, control triggerqueue.StartControl) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	if result, handled, err := u.durableTriggers.cancelTypedQueueStart(ctx, control); handled || err != nil {
		return result, err
	}
	found, err := u.durableTriggers.observeControlledStart(ctx, control)
	if err != nil || !found {
		return requested, err
	}
	outcome, err := u.controlledTerminal(ctx, control)
	if err != nil || outcome.State != startcontrol.CancellationRequested {
		return outcome, err
	}
	if u.cancelPlane == nil || u.cancelPlane.receipts == nil {
		return requested, nil
	}
	digest := sha256.Sum256([]byte(control.Record.ID + "\x00" + control.Cancellation.RequestID))
	_, err = u.cancelPlane.Cancel(ctx, httpapi.CancelRunRequest{IdempotencyKey: "start-queue:" + hex.EncodeToString(digest[:]), RunID: control.Scope.ReservedRunID, Workflow: control.Scope.Workflow, Gaggle: control.Scope.Gaggle, Actor: control.Cancellation.Actor})
	if err != nil {
		return requested, err
	}
	return u.controlledTerminal(ctx, control)
}

func (s *durableTriggerService) observeControlledStart(ctx context.Context, c triggerqueue.StartControl) (bool, error) {
	switch c.Scope.Source {
	case "manual", "schedule", "backlog", "signal":
		if s.ordinary != nil {
			return s.ordinary.Observe(ctx, c.Record)
		}
	case "event":
		if s.events != nil {
			found, _, err := s.events.Observe(ctx, c.Record)
			return found, err
		}
	case "human-restart":
		if s.restarts != nil && s.restarts.Observe != nil {
			plan, err := s.restarts.Load(ctx, c.Record)
			if err != nil {
				return false, err
			}
			return s.restarts.Observe(ctx, plan)
		}
	}
	// Direct Temporal execution requires its exact transport cancellation adapter;
	// a local journal or same-named run can never authorize that effect.
	return false, nil
}

func (u *upSession) controlledTerminal(ctx context.Context, c triggerqueue.StartControl) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	release, ok := u.setup.RunnerRegistry.acquireChildCustody(c.Scope.ReservedRunID)
	if !ok {
		return requested, nil
	}
	defer release()
	dir, err := acceptedTriggerJournalDir(ctx, u.l, c.Scope.ReservedRunID)
	if err != nil || dir == "" {
		return requested, err
	}
	info, err := os.Stat(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return requested, err
	}
	if info.Size() > 32<<20 {
		return requested, errors.New("queue cancellation history exceeds bound")
	}
	writer, report, err := journal.TryRecover(dir)
	if errors.Is(err, journal.ErrRecoveryBusy) {
		return requested, nil
	}
	if err != nil {
		return requested, err
	}
	defer func() { _ = writer.Close() }()
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return requested, err
	}
	id, err := reader.Identity()
	if err != nil {
		return requested, err
	}
	if id.RunID != c.Scope.ReservedRunID || id.Gaggle != c.Scope.Gaggle || id.Workflow != c.Scope.Workflow || id.ConfigGeneration != c.Scope.Generation || id.EngineDriven() {
		return requested, triggerqueue.ErrConflict
	}
	// A released control journal alone does not prove contained workers stopped.
	pending, _, err := pendingParentPodScopes(reader)
	if err != nil || len(pending) > 0 {
		return requested, err
	}
	return controlledTerminalEvents(report.Events, c)
}
func controlledTerminalEvents(events []journal.Event, c triggerqueue.StartControl) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	phase := journal.PhaseFromEvents(events)
	if phase != journal.PhaseCompleted && phase != journal.PhaseFailed && phase != journal.PhaseAborted {
		return requested, nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventRunFinished {
			continue
		}
		if event.Status != string(phase) || event.Seq == 0 || event.Time.IsZero() {
			return requested, errors.New("queue cancellation terminal evidence differs")
		}
		state := startcontrol.CancellationAlreadyTerminal
		if phase == journal.PhaseAborted && !event.Time.Before(c.CancelRequestedAt) {
			state = startcontrol.CancellationConfirmed
		}
		return startcontrol.CancellationObservation{State: state}, nil
	}
	return requested, nil
}
