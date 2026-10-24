package journalpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// OpenHealth acquires the durable health stream without resetting its counter.
func (b *Backend) OpenHealth(
	ctx context.Context,
) (journal.InstanceHandle, journal.OwnerToken, journal.RecoverReport, error) {
	return b.openInstanceStream(ctx, journal.StreamHealth)
}

// HealthAppend commits one globally sequenced health transition.
func (b *Backend) HealthAppend(
	ctx context.Context,
	handle journal.InstanceHandle,
	token journal.OwnerToken,
	event apiv1.GaggleHealthEvent,
) error {
	owner, err := b.instanceOwner(handle, token)
	if err != nil {
		return err
	}
	if owner.stream != journal.StreamHealth ||
		event.OccurredAt.IsZero() || event.Gaggle == "" || event.Type == "" {
		return errors.New("journalpg: invalid health event")
	}
	event.Sequence = 0
	dedupBody, err := json.Marshal(event)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(dedupBody)
	emitKey := hex.EncodeToString(sum[:])
	return b.withInstance(ctx, owner, func(ctx context.Context, tx pgx.Tx, seq int64) error {
		monthStart := monthFloor(event.OccurredAt)
		monthEnd := monthStart.AddDate(0, 1, 0)
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM journal.health_event
				WHERE instance_id=$1 AND stream=$2 AND emit_key=$3
				  AND at>=$4 AND at<$5
			)`,
			b.config.InstanceID, owner.stream, emitKey, monthStart, monthEnd,
		).Scan(&exists); err != nil || exists {
			return err
		}
		if err := tx.QueryRow(ctx, `
			UPDATE journal.instance_seq SET last_seq=last_seq+1
			WHERE instance_id=$1 AND stream=$2
			  AND owner_token=$3 AND owner_epoch=$4
			RETURNING last_seq`,
			b.config.InstanceID, owner.stream, owner.token, owner.epoch,
		).Scan(&seq); err != nil {
			return err
		}
		event.Sequence = uint64(seq)
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO journal.health_event (
				instance_id,stream,seq,at,type,gaggle,emit_key,epoch,body
			)
			SELECT instance_id,stream,$5,$6,$7,$8,$9,owner_epoch,$10
			FROM journal.instance_seq
			WHERE instance_id=$1 AND stream=$2
			  AND owner_token=$3 AND owner_epoch=$4`,
			b.config.InstanceID, owner.stream, owner.token, owner.epoch,
			seq, event.OccurredAt.UTC(), string(event.Type), event.Gaggle,
			emitKey, raw,
		)
		return requireWrite(tag.RowsAffected(), err)
	})
}

// HealthRead returns committed health transitions after a stream cursor.
func (b *Backend) HealthRead(
	ctx context.Context,
	after uint64,
	limit int,
) ([]apiv1.GaggleHealthEvent, error) {
	if after > 1<<63-1 {
		return nil, nil
	}
	bound := int64(limit)
	if bound <= 0 {
		bound = 1<<63 - 1
	}
	rows, err := b.config.Pool.Query(ctx, `
		SELECT body FROM journal.health_event
		WHERE instance_id=$1 AND stream=$2 AND seq>$3
		ORDER BY seq LIMIT $4`,
		b.config.InstanceID, journal.StreamHealth, int64(after), bound,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []apiv1.GaggleHealthEvent
	for rows.Next() {
		var raw []byte
		var event apiv1.GaggleHealthEvent
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func monthFloor(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
}
