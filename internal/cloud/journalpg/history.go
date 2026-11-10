package journalpg

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

var errHistoryChanged = errors.New("journalpg: history advanced during preparation")

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

type tailWrite func(context.Context, pgx.Tx, int64, []journal.Event) error

// withHistory reads the prefix once before the fence. Under the counter lock,
// only the newly committed tail is read/folded. A steady sibling appender cannot
// force full replay retries or starve a lifecycle event.
func (b *Backend) withHistory(ctx context.Context, r *runOwner, prepare func(historySnapshot) (tailWrite, error)) error {
	snapshot, err := b.snapshot(ctx, r.key.RunID)
	if err != nil {
		return err
	}
	write, err := prepare(snapshot)
	if err != nil {
		return err
	}
	observed := snapshot.seq
	return b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, seq int64) error {
		var tail []journal.Event
		if seq > observed {
			rows, err := tx.Query(ctx, `SELECT body FROM journal.run_event WHERE instance_id=$1 AND run_id=$2 AND seq>$3 AND seq<=$4 ORDER BY seq`, b.config.InstanceID, r.key.RunID, observed, seq)
			if err != nil {
				return err
			}
			tail, err = decodeEvents(rows)
			if err != nil {
				return err
			}
		}
		return write(ctx, tx, seq, tail)
	})
}

type runAppend func(context.Context, pgx.Tx, int64) ([]journal.Event, error)

// Ordinary telemetry keeps the constant-size append path. Lifecycle writes
// derive the prefix outside the fence, then fold only the tail and committed
// events (not duplicate emit keys) into their checkpoint under the lock.
func (b *Backend) withAppend(ctx context.Context, r *runOwner, clean []journal.Event, write runAppend) error {
	needsHistory := false
	for _, event := range clean {
		needsHistory = needsHistory || lifecycle(event)
	}
	if !needsHistory {
		return b.withRun(ctx, r, func(ctx context.Context, tx pgx.Tx, seq int64) error { _, err := write(ctx, tx, seq); return err })
	}
	return b.withHistory(ctx, r, func(s historySnapshot) (tailWrite, error) {
		replay := newReplayState(r.key.RunID, s.events)
		return func(ctx context.Context, tx pgx.Tx, seq int64, tail []journal.Event) error {
			replay.fold(tail)
			committed, err := write(ctx, tx, seq)
			if err != nil {
				return err
			}
			replay.fold(committed)
			checkpoint, err := replay.checkpoint()
			if err != nil {
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
