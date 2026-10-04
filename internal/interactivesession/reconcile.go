package interactivesession

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Reconcile observes exact journals and joined writers. It never launches a
// replacement process. Startup alone may grant replay of strongly absent
// journals, before any live execution owner has been installed.
func (s *Service) Reconcile(ctx context.Context, record triggerqueue.Record, allowAbsentReplay bool) error {
	if !s.runtimeReady() {
		return nil
	}
	s.execution.mu.Lock()
	defer s.execution.mu.Unlock()
	t, err := s.Queue.SessionTurn(ctx, record.ID)
	if err != nil {
		return err
	}
	if t.State == "settled" || t.State == "queued" {
		return nil
	}
	owner := s.execution.owners[record.ID]
	if owner != nil && t.Session.State == sessioning.CancelRequested && owner.cancel != nil {
		owner.cancel()
	}
	if owner != nil && !owner.done {
		return nil
	}
	inputs, err := s.Queue.SessionInputs(ctx, record.ID)
	if err != nil {
		return err
	}
	observed, err := s.readObservation(ctx, t, inputs)
	if err != nil {
		return err
	}
	if !observed.Found {
		return s.reconcileAbsent(ctx, t, owner, observed.Absent, allowAbsentReplay)
	}
	if owner == nil {
		release, restoreErr := s.Runtime.Restore(observed.Identity)
		if restoreErr != nil {
			return restoreErr
		}
		owner = &executionOwner{releaseCapacity: release, done: true}
		if s.execution.owners == nil {
			s.execution.owners = map[string]*executionOwner{}
		}
		s.execution.owners[record.ID] = owner
	}
	if t.State == "dispatching" {
		if err = s.Queue.ObserveSessionRun(ctx, t.Record.ID, observed.Identity.RunID, s.Now()); err != nil {
			return err
		}
	}
	if !observed.Terminal || !observed.WritersJoined {
		return nil
	}
	text, err := s.clean(observed.Text, sessioning.MaxTextBytes, true)
	if err != nil {
		return err
	}
	if err = s.Queue.CompleteSessionTurn(ctx, t.Record.ID, triggerqueue.SessionCompletion{RunID: observed.Identity.RunID, Outcome: observed.Outcome, Text: text}, s.Now()); err != nil {
		return err
	}
	releaseOwner(owner)
	delete(s.execution.owners, record.ID)
	return nil
}

func (s *Service) reconcileAbsent(ctx context.Context, t triggerqueue.SessionTurn, owner *executionOwner, absent, replay bool) error {
	if !absent {
		return nil
	}
	if t.State != "dispatching" || t.Record.RunID != "" {
		return errors.New("interactive session published journal is missing")
	}
	if owner != nil && owner.done {
		outcome, text := "rejected", "The turn could not start. Submit another message to retry."
		if t.Session.State == sessioning.CancelRequested {
			outcome, text = "cancelled", "The turn was cancelled before execution."
		}
		if err := s.Queue.CompleteSessionTurn(ctx, t.Record.ID, triggerqueue.SessionCompletion{Outcome: outcome, Text: text}, s.Now()); err != nil {
			return err
		}
		releaseOwner(owner)
		delete(s.execution.owners, t.Record.ID)
		return nil
	}
	if replay && owner == nil {
		return s.Queue.RequeueSessionTurn(ctx, t.Record.ID, "startup verified no session journal", s.Now())
	}
	return nil
}

// Sweep visits bounded unsettled custody before new dispatch and prunes only
// closed, settled records. The daemon retains and rotates the returned cursor.
// Startup must reconcile the entire unsettled inventory before opening new
// admission; a partial scan must not leave unknown workers uncounted.
func (s *Service) Sweep(ctx context.Context, after string) (string, error) {
	ids, err := s.Queue.UnsettledSessionTurns(ctx, after, 100)
	if err != nil {
		return after, err
	}
	if len(ids) == 0 {
		after = ""
	}
	var failures error
	for _, id := range ids {
		if ctx.Err() != nil {
			return after, errors.Join(failures, ctx.Err())
		}
		after = id
		t, readErr := s.Queue.SessionTurn(ctx, id)
		if readErr != nil {
			failures = errors.Join(failures, readErr)
			continue
		}
		failures = errors.Join(failures, s.Reconcile(ctx, t.Record, false))
	}
	_, err = s.Queue.PruneSessions(ctx, s.Now(), 100)
	return after, errors.Join(failures, err)
}
