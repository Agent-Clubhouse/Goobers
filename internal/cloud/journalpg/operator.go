package journalpg

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/jackc/pgx/v5"
)

// AcceptOperatorMessage records a request once by its canonical idempotency key.
func (b *Backend) AcceptOperatorMessage(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, request apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRecord, bool, error) {
	return b.requestOperator(ctx, h, o, request, "", "")
}

// RejectOperatorMessage durably records a pre-acceptance denial.
func (b *Backend) RejectOperatorMessage(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, request apiv1.OperatorMessageRequest, code, detail string) (apiv1.OperatorMessageRecord, bool, error) {
	if code == "" {
		return apiv1.OperatorMessageRecord{}, false, errors.New("journalpg: rejection code required")
	}
	return b.requestOperator(ctx, h, o, request, code, detail)
}
func (b *Backend) requestOperator(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, request apiv1.OperatorMessageRequest, code, detail string) (apiv1.OperatorMessageRecord, bool, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	request, err = journal.PrepareOperatorMessageRequest(r.options.Scrubber, request)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	event := journal.Event{Type: journal.EventOperatorMessageRequested, OperatorMessageRequest: &request}
	accepted := true
	now := r.options.Clock()
	if code != "" || (request.ExpiresAt != nil && !request.ExpiresAt.After(now)) {
		status := apiv1.OperatorMessageRejected
		if code == "" {
			status = apiv1.OperatorMessageExpired
			code = "request_expired"
			accepted = false
		}
		outcome := apiv1.OperatorMessageOutcome{Schema: apiv1.OperatorMessageOutcomeSchema, RequestID: request.RequestID, IdempotencyKey: request.IdempotencyKey, CompletedAt: now, Status: status, Code: code, Detail: detail, Request: &request}
		outcome, err = journal.PrepareOperatorMessageOutcome(r.options.Scrubber, outcome)
		if err != nil {
			return apiv1.OperatorMessageRecord{}, false, err
		}
		event = journal.Event{Type: journal.EventOperatorMessageOutcome, OperatorMessageOutcome: &outcome}
	}
	clean, err := prepareEvent(r, event)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	var result apiv1.OperatorMessageRecord
	appended := false
	var last int64
	err = b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, seq int64) error {
		events, err := b.eventsTx(ctx, tx, r.key.RunID)
		if err != nil {
			return err
		}
		for _, record := range journal.ReplayOperatorMessages(events) {
			if record.Request.IdempotencyKey == request.IdempotencyKey {
				result = record
				return nil
			}
		}
		var committed journal.Event
		last, committed, err = b.appendClean(ctx, tx, r, seq, clean)
		if err != nil {
			return err
		}
		records := journal.ReplayOperatorMessages([]journal.Event{committed})
		if len(records) != 1 {
			return errors.New("journalpg: invalid operator request replay")
		}
		result = records[0]
		appended = true
		return nil
	})
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	if appended {
		b.changed(r, uint64(last))
	}
	return result, appended && accepted, nil
}

// AcknowledgeOperatorMessage commits at most one acknowledgement.
func (b *Backend) AcknowledgeOperatorMessage(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, ack apiv1.OperatorMessageAcknowledgement) (apiv1.OperatorMessageRecord, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	ack, err = journal.PrepareOperatorMessageAcknowledgement(r.options.Scrubber, ack)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	return b.updateOperator(ctx, r, ack.RequestID, ack.IdempotencyKey, journal.Event{Type: journal.EventOperatorMessageAcknowledged, OperatorMessageAcknowledgement: &ack})
}

// CompleteOperatorMessage commits the first terminal delivery disposition.
func (b *Backend) CompleteOperatorMessage(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, outcome apiv1.OperatorMessageOutcome) (apiv1.OperatorMessageRecord, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	if outcome.Request != nil {
		return apiv1.OperatorMessageRecord{}, errors.New("journalpg: accepted outcome must not embed request")
	}
	outcome, err = journal.PrepareOperatorMessageOutcome(r.options.Scrubber, outcome)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	return b.updateOperator(ctx, r, outcome.RequestID, outcome.IdempotencyKey, journal.Event{Type: journal.EventOperatorMessageOutcome, OperatorMessageOutcome: &outcome})
}
func (b *Backend) updateOperator(ctx context.Context, r *runOwner, id, key string, event journal.Event) (apiv1.OperatorMessageRecord, error) {
	clean, err := prepareEvent(r, event)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	var result apiv1.OperatorMessageRecord
	var last int64
	appended := false
	err = b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, seq int64) error {
		events, err := b.eventsTx(ctx, tx, r.key.RunID)
		if err != nil {
			return err
		}
		current, err := findOperator(journal.ReplayOperatorMessages(events), id, key)
		if err != nil {
			return err
		}
		result = current
		if current.Outcome != nil {
			if clean.Type == journal.EventOperatorMessageAcknowledged {
				return journal.ErrOperatorMessageTerminal
			}
			return nil
		}
		if clean.Type == journal.EventOperatorMessageAcknowledged && current.Acknowledgement != nil {
			return nil
		}
		var committed journal.Event
		last, committed, err = b.appendClean(ctx, tx, r, seq, clean)
		if err != nil {
			return err
		}
		result, err = findOperator(journal.ReplayOperatorMessages(append(events, committed)), id, key)
		appended = err == nil
		return err
	})
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	if appended {
		b.changed(r, uint64(last))
	}
	return result, nil
}
func findOperator(records []apiv1.OperatorMessageRecord, id, key string) (apiv1.OperatorMessageRecord, error) {
	for _, record := range records {
		if record.Request.IdempotencyKey == key {
			if record.Request.RequestID != id {
				return apiv1.OperatorMessageRecord{}, fmt.Errorf("journalpg: idempotency key belongs to another request")
			}
			return record, nil
		}
	}
	return apiv1.OperatorMessageRecord{}, journal.ErrOperatorMessageNotFound
}
