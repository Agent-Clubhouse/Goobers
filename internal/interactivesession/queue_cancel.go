package interactivesession

import (
	"context"
	"strings"

	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// CancelQueuedTurn is a host-only adapter invoked while current queue.cancel
// authority is held. It stops only this accepted turn, leaving the conversation
// and later queued messages open. It never acquires the human policy lock.
func (s *Service) CancelQueuedTurn(ctx context.Context, gaggle, acceptance string) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	if err := s.ready(); err != nil {
		return requested, err
	}
	c, err := s.Queue.StartControl(ctx, gaggle, acceptance)
	if err != nil {
		return requested, err
	}
	if c.Cancellation == nil || c.Scope.Source != "session" || c.Scope.ReservedRunID != strings.TrimPrefix(acceptance, "trigger-") {
		return requested, triggerqueue.ErrConflict
	}
	// Dispatch may hold this lock while entering current human policy. Waiting
	// here under that policy's read lock could deadlock a pending policy update.
	if !s.runtimeReady() || !s.execution.mu.TryLock() {
		return requested, nil
	}
	defer s.execution.mu.Unlock()
	t, err := s.Queue.SessionTurn(ctx, acceptance)
	if err != nil {
		return requested, err
	}
	if t.Session.Gaggle != gaggle || c.Scope.Workflow != "session/"+t.Session.ID || c.Scope.Generation != t.Session.ConfigGeneration {
		return requested, triggerqueue.ErrConflict
	}
	if owner := s.execution.owners[acceptance]; owner != nil && !owner.done && owner.cancel != nil {
		owner.cancelRequested = true
		owner.cancel()
	}
	if err = s.reconcileTurnLocked(ctx, t.Record, false); err != nil {
		return requested, err
	}
	return s.observeQueuedTurnCancellation(ctx, c)
}

func (s *Service) observeQueuedTurnCancellation(ctx context.Context, c triggerqueue.StartControl) (startcontrol.CancellationObservation, error) {
	requested := startcontrol.CancellationObservation{State: startcontrol.CancellationRequested}
	t, err := s.Queue.SessionTurn(ctx, c.Record.ID)
	if err != nil || t.State != "settled" {
		return requested, err
	}
	// The ordinary reconciliation path settled exact strong absence after the
	// cancelled owner joined, or the queue settled a proven unattempted turn.
	if t.Record.State == triggerqueue.Rejected && t.Record.RunID == "" && t.Outcome == "cancelled" {
		return startcontrol.CancellationObservation{State: startcontrol.CancellationConfirmed}, nil
	}
	inputs, err := s.Queue.SessionInputs(ctx, c.Record.ID)
	if err != nil {
		return requested, err
	}
	observed, err := s.readObservation(ctx, t, inputs)
	if err != nil || !observed.Found || !observed.Terminal || !observed.WritersJoined || observed.TerminalAt.IsZero() {
		return requested, err
	}
	state := startcontrol.CancellationAlreadyTerminal
	if observed.Outcome == "cancelled" && !observed.TerminalAt.Before(c.CancelRequestedAt) {
		state = startcontrol.CancellationConfirmed
	}
	return startcontrol.CancellationObservation{State: state}, nil
}
