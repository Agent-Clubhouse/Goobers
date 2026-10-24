package journalpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

// LoadStreamCursor reads one consumer's durable (stream, sequence) position.
func (b *Backend) LoadStreamCursor(
	ctx context.Context,
	consumer string,
	stream string,
) (journal.StreamCursor, error) {
	if consumer == "" || stream == "" {
		return journal.StreamCursor{}, errors.New("journalpg: invalid consumer cursor")
	}
	cursor := journal.StreamCursor{Stream: stream}
	err := b.config.Pool.QueryRow(ctx, `
		SELECT seq FROM journal.consumer_cursor
		WHERE instance_id=$1 AND consumer=$2 AND stream=$3`,
		b.config.InstanceID, consumer, stream,
	).Scan(&cursor.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return cursor, nil
	}
	return cursor, err
}

// AdvanceStreamCursor performs an exact monotonic CAS.
func (b *Backend) AdvanceStreamCursor(
	ctx context.Context,
	consumer string,
	current journal.StreamCursor,
	next uint64,
) error {
	if consumer == "" || current.Stream == "" || next < current.Seq ||
		next > 1<<63-1 || current.Seq > 1<<63-1 {
		return errors.New("journalpg: invalid consumer cursor")
	}
	var advanced bool
	err := b.config.Pool.QueryRow(ctx, `
		WITH updated AS (
			UPDATE journal.consumer_cursor
			SET seq=$4,updated_at=clock_timestamp()
			WHERE instance_id=$1 AND consumer=$2 AND stream=$3
			  AND seq=$5 AND seq<=$4
			RETURNING 1
		), inserted AS (
			INSERT INTO journal.consumer_cursor (
				instance_id,consumer,stream,seq
			)
			SELECT $1,$2,$3,$4
			WHERE $5=0 AND NOT EXISTS (SELECT 1 FROM updated)
			ON CONFLICT DO NOTHING
			RETURNING 1
		)
		SELECT EXISTS(SELECT 1 FROM updated UNION ALL SELECT 1 FROM inserted)`,
		b.config.InstanceID, consumer, current.Stream, int64(next), int64(current.Seq),
	).Scan(&advanced)
	if err != nil {
		return err
	}
	if !advanced {
		return journal.ErrStreamCursorConflict
	}
	return nil
}

var _ journal.StreamCursorStore = (*Backend)(nil)
