package interactivesession

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Dispatch takes only an already accepted queue receipt. Browser contexts never
// own execution. Admission cancellation before handoff preserves queued input;
// once handed off the independent daemon execution context owns the worker.
func (s *Service) Dispatch(admission, execution context.Context, record triggerqueue.Record) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !s.runtimeReady() {
		return nil
	}
	s.execution.mu.Lock()
	defer s.execution.mu.Unlock()
	t, err := s.Queue.SessionTurn(admission, record.ID)
	if err != nil {
		return err
	}
	if !bytes.Equal(t.Record.Payload, record.Payload) {
		return triggerqueue.ErrConflict
	}
	if t.State != "queued" || t.Record.State != triggerqueue.Accepted {
		return nil
	}
	inputs, err := s.Queue.SessionInputs(admission, record.ID)
	if errors.Is(err, triggerqueue.ErrTransition) || errors.Is(err, triggerqueue.ErrSessionClosed) {
		return nil
	}
	if err != nil {
		return err
	}
	observed, err := s.readObservation(admission, t, inputs)
	if err != nil {
		return err
	}
	if observed.Found || !observed.Absent {
		return errors.New("interactive session requires custody reconciliation")
	}
	owner, prepared, err := s.prepareTurn(admission, execution, t, inputs)
	if err != nil {
		return err
	}
	claimed, err := s.Queue.BeginSessionTurn(admission, record.ID, s.Now())
	if err != nil {
		releaseOwner(owner)
		return err
	}
	if s.execution.owners == nil {
		s.execution.owners = map[string]*executionOwner{}
	}
	s.execution.owners[record.ID] = owner
	s.execution.workers.Add(1)
	go s.runTurn(claimed, inputs, prepared, owner)
	return nil
}

func (s *Service) prepareTurn(admission, execution context.Context, t triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs) (*executionOwner, PreparedTurn, error) {
	principal, err := turnPrincipal(t.Authority, t.Message.Actor)
	if err != nil {
		return nil, PreparedTurn{}, err
	}
	ctx, cancel := context.WithCancel(execution)
	owner := &executionOwner{cancel: cancel}
	lease, err := s.Permissions.BeginSessionExecution(ctx, principal, t.Session.Gaggle)
	if err != nil {
		cancel()
		if errors.Is(err, interactiveaccess.ErrDenied) {
			err = errors.Join(err, s.Queue.CompleteSessionTurn(admission, t.Record.ID, triggerqueue.SessionCompletion{Outcome: "rejected", Text: "Current gaggle policy does not permit this turn."}, s.Now()))
		}
		return nil, PreparedTurn{}, err
	}
	owner.lease = lease
	prepared, err := s.Runtime.Build(admission, t, inputs)
	owner.releaseSource = prepared.Release
	if err == nil && (prepared.Run == nil || prepared.Release == nil) {
		err = errors.New("interactive session runtime preparation incomplete")
	}
	if err == nil {
		err = verifyTurnIdentity(t, inputs, prepared.Identity)
	}
	if err == nil {
		owner.releaseCapacity, err = s.Runtime.Reserve(admission, prepared.Identity, s.Now())
	}
	if err != nil {
		releaseOwner(owner)
		return nil, PreparedTurn{}, err
	}
	return owner, prepared, nil
}

func (s *Service) runTurn(t triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs, prepared PreparedTurn, owner *executionOwner) {
	defer s.execution.workers.Done()
	// The callback performs an independent host read; a reserved identifier or
	// an executor's claimed identity is insufficient to create a public run link.
	published := func() error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(owner.lease.Context()), 10*time.Second)
		defer cancel()
		o, err := s.readObservation(ctx, t, inputs)
		if err != nil {
			return err
		}
		if !o.Found {
			return errors.New("interactive session journal was not published")
		}
		return s.Queue.ObserveSessionRun(ctx, t.Record.ID, o.Identity.RunID, s.Now())
	}
	_ = prepared.Run(owner.lease.Context(), owner.lease, published)
	s.execution.mu.Lock()
	owner.done = true
	s.execution.mu.Unlock()
	// Cancellation must not discard exact final custody. Failed inspection keeps
	// both the lease and capacity owned; the normal sweep retries observation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(owner.lease.Context()), 10*time.Second)
	defer cancel()
	_ = s.Reconcile(ctx, t.Record, false)
}
