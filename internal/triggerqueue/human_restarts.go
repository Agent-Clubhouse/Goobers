package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const humanRestartSchema = `CREATE TABLE human_restart_plans (
 acceptance_id TEXT PRIMARY KEY NOT NULL,
 gaggle TEXT NOT NULL, source_run TEXT NOT NULL, terminal_seq INTEGER NOT NULL,
 stage TEXT NOT NULL, epoch TEXT NOT NULL UNIQUE,
 plan BLOB NOT NULL CHECK(length(plan) BETWEEN 1 AND 4194304),
 UNIQUE(gaggle,source_run,terminal_seq,stage)
);
CREATE TRIGGER human_restart_delete AFTER DELETE ON triggers BEGIN
 DELETE FROM human_restart_plans WHERE acceptance_id=OLD.id;
END;`

// HumanRestartAcceptance holds host-verified epoch custody, not caller authority.
// Its immutable context consumes the existing ledger's byte and slot limits.
type HumanRestartAcceptance struct {
	Key, Actor, Gaggle, SourceRun, Stage, Epoch string
	TerminalSequence                            uint64
	Payload, Plan                               []byte
}

// AcceptHumanRestart atomically accepts one continuation per source occurrence.
// No capacity reservation or executor call may precede this transaction.
func (s *Store) AcceptHumanRestart(ctx context.Context, r HumanRestartAcceptance, now time.Time) (Record, bool, error) {
	if err := r.validate(now); err != nil {
		return Record{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	prior, err := scanRecord(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE key=?", r.Key))
	if err == nil {
		var saved []byte
		readErr := tx.QueryRowContext(ctx, "SELECT plan FROM human_restart_plans WHERE acceptance_id=?", prior.ID).Scan(&saved)
		if readErr != nil || prior.Actor != r.Actor || string(prior.Payload) != string(r.Payload) || string(saved) != string(r.Plan) {
			return Record{}, false, ErrConflict
		}
		return prior, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, err
	}
	var occupied bool
	err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM human_restart_plans WHERE epoch=? OR (gaggle=? AND source_run=? AND terminal_seq=? AND stage=?))", r.Epoch, r.Gaggle, r.SourceRun, r.TerminalSequence, r.Stage).Scan(&occupied)
	if err != nil {
		return Record{}, false, err
	}
	if occupied {
		return Record{}, false, ErrConflict
	}
	if err = triggerSlotCapacity(ctx, tx, 1); err != nil {
		return Record{}, false, err
	}
	if err = childByteCapacity(ctx, tx, len(r.Plan)+len(r.Payload)+len(r.Key)+len(r.Actor)+2048); err != nil {
		return Record{}, false, err
	}
	record := Record{ID: fmt.Sprintf("trigger-%x", randomID()), Key: r.Key, Actor: r.Actor, Payload: append([]byte(nil), r.Payload...), State: Accepted, AcceptedAt: now.UTC()}
	_, err = tx.ExecContext(ctx, "INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES(?,?,?,?,?,?)", record.ID, r.Key, r.Actor, r.Payload, Accepted, now.UnixNano())
	if err != nil {
		return Record{}, false, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO human_restart_plans(acceptance_id,gaggle,source_run,terminal_seq,stage,epoch,plan) VALUES(?,?,?,?,?,?,?)", record.ID, r.Gaggle, r.SourceRun, r.TerminalSequence, r.Stage, r.Epoch, r.Plan)
	if err != nil {
		return Record{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return record, false, nil
}

// HumanRestartPlan returns the host-owned bounded snapshot for one receipt.
func (s *Store) HumanRestartPlan(ctx context.Context, id string) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT plan FROM human_restart_plans WHERE acceptance_id=?", id).Scan(&raw)
	return raw, err
}

func (r HumanRestartAcceptance) validate(now time.Time) error {
	if !validChildText(r.Key, 256, true) || !validChildText(r.Actor, 1024, true) || len(r.Payload) == 0 || len(r.Payload) > MaxPayloadBytes || now.IsZero() {
		return ErrTransition
	}
	if !validChildText(r.Gaggle, 256, true) || !validChildText(r.SourceRun, 256, true) || !validChildText(r.Stage, 256, true) || !validChildText(r.Epoch, 256, true) || r.SourceRun == r.Epoch {
		return ErrTransition
	}
	if r.TerminalSequence == 0 || r.TerminalSequence > 1<<63-1 || len(r.Plan) == 0 || len(r.Plan) > 4<<20 {
		return ErrTransition
	}
	return nil
}
