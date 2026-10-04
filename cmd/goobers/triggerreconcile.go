package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// acceptedTriggerObserver only acknowledges a matching, durably published run
// identity. Missing, corrupt, ambiguous or mismatched journals never authorize
// replay here. Startup reconciliation of absent runs is a separate decision.
func acceptedTriggerObserver(layout instance.Layout) func(context.Context, triggerqueue.Record) (bool, error) {
	return func(ctx context.Context, record triggerqueue.Record) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		runID := strings.TrimPrefix(record.ID, "trigger-")
		if record.RunID != "" && record.RunID != runID {
			return false, fmt.Errorf("accepted trigger %s has a mismatched dispatch receipt", record.ID)
		}
		dir, err := acceptedTriggerJournalDir(ctx, layout, runID)
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
		identity, err := reader.Identity()
		if err != nil {
			return false, err
		}
		var header struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(record.Payload, &header); err != nil {
			return false, err
		}
		if header.Kind == startintent.Kind {
			err := startintent.VerifyIdentity(identity, record)
			return err == nil, err
		}
		var payload acceptedTriggerPayload
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return false, err
		}
		if identity.RunID != runID || identity.Workflow != payload.Request.Workflow || (payload.Request.Gaggle != "" && identity.Gaggle != payload.Request.Gaggle) {
			return false, fmt.Errorf("accepted trigger %s has a mismatched run identity", record.ID)
		}
		return true, nil
	}
}

func (s *durableTriggerService) reconcileObserved(ctx context.Context) error {
	if s.observe == nil && s.observeChild == nil && s.events == nil && s.ordinary == nil && s.sessions == nil {
		return nil
	}
	records, err := s.queue.Uncertain(ctx, s.reconcileCursor, 100)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		s.reconcileCursor = ""
		return nil
	}
	var failures error
	for _, record := range records {
		s.reconcileCursor = record.ID
		failures = errors.Join(failures, s.reconcileAcceptedRecord(ctx, record))
	}
	return failures
}

func (s *durableTriggerService) reconcileObservation(ctx context.Context, record triggerqueue.Record, observed bool) error {
	var err error
	switch {
	case observed:
		err = s.queue.Finish(ctx, record.ID, triggerqueue.Dispatched, strings.TrimPrefix(record.ID, "trigger-"), "", s.dispatch.now())
	case s.bootUncertain[record.ID]:
		// The daemon owns up.lock and captured this set before its first drain.
		// Both starter backends publish a journal before accepted execution;
		// retention records a receipt before removing that proof. Strong
		// absence for this prior-process record therefore permits retry.
		err = s.queue.RetryUnstarted(ctx, record.ID)
	default:
		return nil
	}
	if err == nil || errors.Is(err, triggerqueue.ErrTransition) {
		delete(s.bootUncertain, record.ID)
		return nil
	}
	return err
}

// Route by the durable discriminant before consulting an ordinary observer.
func (s *durableTriggerService) reconcileAcceptedRecord(ctx context.Context, record triggerqueue.Record) error {
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(record.Payload, &header); err != nil {
		return err
	}
	if header.Kind == startintent.Kind {
		if s.ordinary == nil {
			return nil
		}
		err := s.ordinary.Reconcile(ctx, record, s.bootUncertain[record.ID])
		if err == nil || errors.Is(err, triggerqueue.ErrTransition) {
			delete(s.bootUncertain, record.ID)
			return nil
		}
		return err
	}
	if header.Kind == childworkflow.ChildStartKind {
		return s.reconcileChildReceipt(ctx, record)
	}
	if header.Kind == sessioning.StartKind {
		if s.sessions == nil {
			return nil
		}
		err := s.sessions.Reconcile(ctx, record, s.bootUncertain[record.ID])
		if err == nil {
			delete(s.bootUncertain, record.ID)
		}
		return err
	}
	if header.Kind == eventing.StartKind {
		if s.events == nil {
			return nil
		}
		err := s.events.Reconcile(ctx, record, s.bootUncertain[record.ID])
		if err == nil || errors.Is(err, triggerqueue.ErrTransition) {
			delete(s.bootUncertain, record.ID)
			return nil
		}
		return err
	}
	if header.Kind != "" {
		return errors.New("unsupported accepted trigger envelope kind")
	}
	if s.observe == nil {
		return nil
	}
	observed, err := s.observe(ctx, record)
	if err != nil {
		return err
	}
	return s.reconcileObservation(ctx, record, observed)
}
