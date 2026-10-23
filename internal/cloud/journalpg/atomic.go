package journalpg

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/jackc/pgx/v5"
)

// ClaimNotificationDelivery resolves a durable sink claim under the append lock.
func (b *Backend) ClaimNotificationDelivery(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, receipt apiv1.NotificationReceipt) (*apiv1.NotificationReceipt, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return nil, err
	}
	clean, err := prepareEvent(r, journal.Event{Type: journal.EventNotificationReceipt, NotificationReceipt: &receipt})
	if err != nil {
		return nil, err
	}
	var existing *apiv1.NotificationReceipt
	var last int64
	err = b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, seq int64) error {
		events, err := b.eventsTx(ctx, tx, r.key.RunID)
		if err != nil {
			return err
		}
		for i := len(events) - 1; i >= 0; i-- {
			prior := events[i].NotificationReceipt
			if prior == nil || prior.IdempotencyDigest != clean.NotificationReceipt.IdempotencyDigest || prior.Sink.Kind != clean.NotificationReceipt.Sink.Kind || prior.Attempt == 0 {
				continue
			}
			if prior.Status == apiv1.NotificationDelivered || prior.Status == apiv1.NotificationPending || prior.Unresolved {
				existing = prior
				return nil
			}
			break
		}
		last, _, err = b.appendClean(ctx, tx, r, seq, clean)
		return err
	})
	if err != nil {
		return nil, err
	}
	if existing == nil {
		b.changed(r, uint64(last))
	}
	return existing, nil
}

// AppendChildStageStarted binds lineage to the database-allocated sequence.
func (b *Backend) AppendChildStageStarted(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, record journal.ChildStageStartRecord) (uint64, *apiv1.ChildWorkflowOrigin, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return 0, nil, err
	}
	if record.Event.Type != journal.EventStageStarted || record.Event.Stage == "" || record.Event.Attempt < 1 {
		return 0, nil, errors.New("journalpg: child origin requires a stage start")
	}
	clean, err := prepareEvent(r, record.Event)
	if err != nil {
		return 0, nil, err
	}
	var committed journal.Event
	err = b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, seq int64) error {
		attempt := journal.StageAttemptID(r.key.RunID, clean.Branch, clean.Stage, uint64(seq+1))
		occurrence := attempt
		if record.Continuation {
			var err error
			occurrence, err = b.previousOccurrence(ctx, tx, r, clean)
			if err != nil {
				return err
			}
		}
		if clean.Runner == nil {
			clean.Runner = map[string]any{}
		}
		clean.Runner[journal.ChildWorkflowOccurrenceKey] = occurrence
		clean.Runner[journal.ChildWorkflowAttemptKey] = attempt
		if _, ok := clean.Runner["artifactVisit"]; ok {
			clean.Runner["artifactVisit"] = uint64(seq + 1)
		}
		var err error
		_, committed, err = b.appendClean(ctx, tx, r, seq, clean)
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	origin, err := journal.ChildWorkflowOriginForEvent(r.key.RunID, committed)
	if err != nil {
		return 0, nil, err
	}
	b.changed(r, committed.Seq)
	return committed.Seq, origin, nil
}
func (b *Backend) previousOccurrence(ctx context.Context, tx pgx.Tx, r *runOwner, event journal.Event) (string, error) {
	events, err := b.eventsTx(ctx, tx, r.key.RunID)
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		prior := events[i]
		if prior.Type != journal.EventStageStarted || prior.Stage != event.Stage || prior.Branch != event.Branch {
			continue
		}
		origin, err := journal.ChildWorkflowOriginForEvent(r.key.RunID, prior)
		if err != nil {
			return "", err
		}
		return origin.StageOccurrence, nil
	}
	return "", fmt.Errorf("journalpg: child continuation has no bound occurrence")
}
