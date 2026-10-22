package planes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// ReconcileObserved reconciles uncertain acceptances against durable publication.
func (s *DurableTriggerService) ReconcileObserved(ctx context.Context) error {
	if s.observe == nil && !s.backend.CanReconcileAccepted() {
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
		failures = errors.Join(failures, s.ReconcileAcceptedRecord(ctx, record))
	}
	return failures
}

// ReconcileObservation applies one durable publication observation.
func (s *DurableTriggerService) ReconcileObservation(ctx context.Context, record triggerqueue.Record, observed bool) error {
	var err error
	switch {
	case observed:
		err = s.queue.Finish(ctx, record.ID, triggerqueue.Dispatched, strings.TrimPrefix(record.ID, "trigger-"), "", s.dispatch.Now())
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

// ReconcileAcceptedRecord routes by the durable discriminant before consulting an ordinary observer.
func (s *DurableTriggerService) ReconcileAcceptedRecord(ctx context.Context, record triggerqueue.Record) error {
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(record.Payload, &header); err != nil {
		return err
	}
	if header.Kind != "" {
		observed, err := s.backend.ReconcileAccepted(ctx, record, header.Kind)
		if err != nil || observed == nil {
			return err
		}
		return s.ReconcileObservation(ctx, record, *observed)
	}
	if s.observe == nil {
		return nil
	}
	observed, err := s.observe(ctx, record)
	if err != nil {
		return err
	}
	return s.ReconcileObservation(ctx, record, observed)
}

// BootUncertain reports whether an acceptance predates this process.
func (s *DurableTriggerService) BootUncertain(id string) bool { return s.bootUncertain[id] }

// ClearBootUncertain removes one reconciled boot-time uncertainty.
func (s *DurableTriggerService) ClearBootUncertain(id string) { delete(s.bootUncertain, id) }

// BootUncertainCount reports unreconciled boot-time acceptances.
func (s *DurableTriggerService) BootUncertainCount() int { return len(s.bootUncertain) }
