package journalpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
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
		prior, err := b.latestNotification(ctx, tx, r.key.RunID, *clean.NotificationReceipt)
		if err != nil {
			return err
		}
		if prior != nil && (prior.Status == apiv1.NotificationDelivered || prior.Status == apiv1.NotificationPending || prior.Unresolved) {
			existing = prior
			return nil
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
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT body FROM journal.run_event WHERE instance_id=$1 AND run_id=$2 AND stage=$3 AND branch=$4 AND type='stage.started' ORDER BY seq DESC LIMIT 1`, b.config.InstanceID, r.key.RunID, event.Stage, event.Branch).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("journalpg: child continuation has no bound occurrence")
	}
	if err != nil {
		return "", err
	}
	var prior journal.Event
	if err := json.Unmarshal(raw, &prior); err != nil {
		return "", err
	}
	origin, err := journal.ChildWorkflowOriginForEvent(r.key.RunID, prior)
	if err != nil {
		return "", err
	}
	return origin.StageOccurrence, nil
}

func notificationIndex(event journal.Event) (string, string, int) {
	if receipt := event.NotificationReceipt; receipt != nil {
		return receipt.IdempotencyDigest, string(receipt.Sink.Kind), receipt.Attempt
	}
	return "", "", 0
}
func (b *Backend) latestNotification(ctx context.Context, tx pgx.Tx, runID string, receipt apiv1.NotificationReceipt) (*apiv1.NotificationReceipt, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT body FROM journal.run_event WHERE instance_id=$1 AND run_id=$2 AND notification_key=$3 AND notification_sink=$4 AND notification_attempt<>0 ORDER BY seq DESC LIMIT 1`, b.config.InstanceID, runID, receipt.IdempotencyDigest, string(receipt.Sink.Kind)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var event journal.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	return event.NotificationReceipt, nil
}
