package journalpg

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

var errHistoryChanged = errors.New("journalpg: history advanced during preparation")

type runWrite func(context.Context, pgx.Tx, int64) error

type historySnapshot struct {
	seq    int64
	events []journal.Event
}

// snapshot reads immutable committed bytes through an observed counter. It
// neither holds the append lock nor consumes the two-second write budget.
func (b *Backend) snapshot(ctx context.Context, runID string) (historySnapshot, error) {
	var s historySnapshot
	if err := b.config.Pool.QueryRow(ctx, `SELECT last_seq FROM journal.run_seq WHERE instance_id=$1 AND run_id=$2`, b.config.InstanceID, runID).Scan(&s.seq); err != nil {
		return s, err
	}
	rows, err := b.config.Pool.Query(ctx, `SELECT body FROM journal.run_event WHERE instance_id=$1 AND run_id=$2 AND seq<=$3 ORDER BY seq`, b.config.InstanceID, runID, s.seq)
	if err != nil {
		return s, err
	}
	s.events, err = decodeEvents(rows)
	return s, err
}

func decodeEvents(rows pgx.Rows) ([]journal.Event, error) {
	defer rows.Close()
	var events []journal.Event
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var event journal.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// withHistory evaluates arbitrary replay/predicate work before WithFence. A
// counter comparison under the writer lock makes that decision atomic with its
// writes. Retried preparation must have no side effects. No history is decoded
// or scanned in the fenced callback, even when the run has millions of events.
func (b *Backend) withHistory(ctx context.Context, r *runOwner, prepare func(historySnapshot) (runWrite, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := b.snapshot(ctx, r.key.RunID)
		if err != nil {
			return err
		}
		write, err := prepare(snapshot)
		if err != nil {
			return err
		}
		err = b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, seq int64) error {
			if seq != snapshot.seq {
				return errHistoryChanged
			}
			return write(ctx, tx, seq)
		})
		if !errors.Is(err, errHistoryChanged) {
			return err
		}
	}
}

// withAppend only replays history for events that actually need it. Ordinary
// telemetry, blobs and receipts keep the constant-size indexed append path.
func (b *Backend) withAppend(ctx context.Context, r *runOwner, clean []journal.Event, write runWrite) error {
	needsHistory := false
	for _, event := range clean {
		needsHistory = needsHistory || lifecycle(event)
	}
	if !needsHistory {
		return b.withRun(ctx, r, write)
	}
	return b.withHistory(ctx, r, func(s historySnapshot) (runWrite, error) {
		checkpoint, err := prepareAppendCheckpoint(r.key.RunID, s, clean)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, tx pgx.Tx, seq int64) error {
			if err := write(ctx, tx, seq); err != nil {
				return err
			}
			return b.storeCheckpoint(ctx, tx, r, checkpoint)
		}, nil
	})
}

type preparedCheckpoint struct {
	state journal.State
	raw   []byte
}

func prepareCheckpoint(runID string, events []journal.Event) (*preparedCheckpoint, error) {
	state := stateForEvents(runID, events)
	raw, err := json.Marshal(state)
	return &preparedCheckpoint{state: state, raw: raw}, err
}

// Simulate only allocation/dedup for the snapshot's next batch. The counter is
// rechecked before use; appendClean still allocates each real sequence in SQL.
func prepareAppendCheckpoint(runID string, s historySnapshot, clean []journal.Event) (*preparedCheckpoint, error) {
	keys := map[any]bool{}
	for _, event := range s.events {
		if key := emitKey(event); key != nil {
			keys[key] = true
		}
	}
	events := s.events
	checkpoint := false
	for _, event := range clean {
		key := emitKey(event)
		if key != nil && keys[key] {
			continue
		}
		if key != nil {
			keys[key] = true
		}
		s.seq++
		event.Seq = uint64(s.seq)
		events = append(events, event)
		checkpoint = checkpoint || lifecycle(event)
	}
	if !checkpoint {
		return nil, nil
	}
	return prepareCheckpoint(runID, events)
}

func (b *Backend) storeCheckpoint(ctx context.Context, tx pgx.Tx, r *runOwner, checkpoint *preparedCheckpoint) error {
	if checkpoint == nil {
		return nil
	}
	state := checkpoint.state
	tag, err := tx.Exec(ctx, `INSERT INTO journal.run_checkpoint(instance_id,run_id,phase,terminal,state,at_seq)
 SELECT instance_id,run_id,$5,$6,$7,$8 FROM journal.run_seq WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4
 ON CONFLICT(instance_id,run_id) DO UPDATE SET phase=EXCLUDED.phase,terminal=EXCLUDED.terminal,state=EXCLUDED.state,at_seq=EXCLUDED.at_seq
 WHERE EXISTS(SELECT 1 FROM journal.run_seq s WHERE s.instance_id=$1 AND s.run_id=$2 AND s.owner_token=$3 AND s.owner_epoch=$4)`, b.config.InstanceID, r.key.RunID, r.token, r.epoch, string(state.Phase), state.Phase != journal.PhaseRunning, checkpoint.raw, int64(state.LastSeq))
	return requireWrite(tag.RowsAffected(), err)
}
