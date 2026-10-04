package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

// CloseSession fences intake and cancels queued turns in the same transaction.
// Active/uncertain dispatch remains owned until an actual stop/result receipt.
func (s *Store) CloseSession(ctx context.Context, c SessionCommand, id, reason string, now time.Time) (sessioning.Acceptance, error) {
	if !c.valid() || len(reason) > 4096 || now.IsZero() {
		return sessioning.Acceptance{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if result, found, err := sessionReplay(ctx, tx, c, "close", id); err != nil || found {
		return result, err
	}
	session, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM interactive_sessions WHERE gaggle=? AND id=?", c.Gaggle, id))
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	if err = sessionCapacity(ctx, tx, c.Gaggle, 1); err != nil {
		return sessioning.Acceptance{}, err
	}
	if err = childByteCapacity(ctx, tx, len(reason)+8192); err != nil {
		return sessioning.Acceptance{}, err
	}
	if err = cancelQueuedSessionTurns(ctx, tx, id, now); err != nil {
		return sessioning.Acceptance{}, err
	}
	state := sessioning.CancelRequested
	var closed any
	if session.ActiveTurnID == "" {
		state = sessioning.Closed
		closed = now.UnixNano()
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_sessions SET state=?,closed_ns=COALESCE(closed_ns,?),updated_ns=? WHERE id=?`, state, closed, now.UnixNano(), id)
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	if session.State != sessioning.Closed && session.State != sessioning.CancelRequested {
		if reason == "" {
			reason = "Session close requested."
		}
		actor, _ := json.Marshal(c.Actor)
		_, err = tx.ExecContext(ctx, `INSERT INTO interactive_messages(id,session_id,sequence,actor_kind,actor,text,created_ns,outcome) VALUES(?,?,?,'human',?,?,?,'close-requested')`, fmt.Sprintf("message-%x", randomID()), id, session.NextSequence, actor, reason, now.UnixNano())
		if err != nil {
			return sessioning.Acceptance{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE interactive_sessions SET next_sequence=next_sequence+1 WHERE id=?`, id); err != nil {
			return sessioning.Acceptance{}, err
		}
	}
	if err = keepSessionRequest(ctx, tx, c, "close", id, id, "", "", now); err != nil {
		return sessioning.Acceptance{}, err
	}
	result, err := sessionAcceptance(ctx, tx, c.Gaggle, id, "", "")
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func cancelQueuedSessionTurns(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE triggers SET state='rejected',reason='session closed before dispatch',finished_ns=? WHERE state='accepted' AND id IN (SELECT acceptance_id FROM interactive_turns WHERE session_id=? AND state='queued')`, now.UnixNano(), id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_messages SET outcome='cancelled' WHERE id IN (SELECT message_id FROM interactive_turns WHERE session_id=? AND state='queued')`, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_turns SET state='settled',outcome='cancelled',settled_ns=?,reserved_bytes=0 WHERE session_id=? AND state='queued'`, now.UnixNano(), id)
	return err
}

// SessionCompletion is observed host evidence. RunID must have already been
// verified by ObserveSessionRun. A refused unstarted turn has no RunID.
type SessionCompletion struct {
	RunID   string
	Outcome string
	Text    string
}

// CompleteSessionTurn appends one response and releases only the exact owned
// turn. Result replay is digest checked; stale outcomes cannot advance the FIFO.
func (s *Store) CompleteSessionTurn(ctx context.Context, acceptance string, result SessionCompletion, now time.Time) error {
	if !validSessionOutcome(result.Outcome) || !validSessionText(result.Text) || now.IsZero() {
		return ErrTransition
	}
	raw, _ := json.Marshal(result)
	digest := "sha256:" + childDigest(raw)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := readSessionTurn(ctx, tx, acceptance)
	if err != nil {
		return err
	}
	if t.State == "settled" {
		var prior string
		err = tx.QueryRowContext(ctx, `SELECT result_digest FROM interactive_turns WHERE id=?`, t.ID).Scan(&prior)
		if err != nil {
			return err
		}
		if prior != digest {
			return ErrConflict
		}
		return nil
	}
	if err = validSessionCompletion(t, result); err != nil {
		return err
	}
	// The maximum response was reserved at acceptance. Other producers see it
	// in shared byte admission until this transaction replaces it with bytes.
	if err = childByteCapacity(ctx, tx, len(result.Text)+8192-sessionResponseAllowance); err != nil {
		return err
	}
	response := fmt.Sprintf("message-%x", randomID())
	kind := "agent"
	if result.RunID == "" {
		kind = "system"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO interactive_messages(id,session_id,sequence,actor_kind,actor,text,created_ns,turn_id,run_id,outcome) VALUES(?,?,?,?,'null',?,?,?,?,?)`, response, t.Session.ID, t.Session.NextSequence, kind, result.Text, now.UnixNano(), t.ID, result.RunID, result.Outcome)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_turns SET state='settled',outcome=?,response_id=?,result_digest=?,settled_ns=?,reserved_bytes=0 WHERE id=?`, result.Outcome, response, digest, now.UnixNano(), t.ID)
	if err != nil {
		return err
	}
	if result.RunID == "" {
		_, err = tx.ExecContext(ctx, `UPDATE triggers SET state='rejected',reason=?,finished_ns=? WHERE id=?`, result.Outcome, now.UnixNano(), acceptance)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE triggers SET finished_ns=? WHERE id=?`, now.UnixNano(), acceptance)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_messages SET outcome=? WHERE id=?`, result.Outcome, t.Message.ID)
	if err != nil {
		return err
	}
	if err = settleSessionOwner(ctx, tx, t, result.Outcome, now); err != nil {
		return err
	}
	return tx.Commit()
}

func validSessionOutcome(v string) bool {
	return v == "success" || v == "failed" || v == "cancelled" || v == "rejected" || v == "needs-human"
}
func validSessionCompletion(t SessionTurn, r SessionCompletion) error {
	if r.RunID != "" {
		if t.State != "running" || t.Record.State != Dispatched || t.Record.RunID != r.RunID || t.Session.ActiveTurnID != t.ID {
			return ErrTransition
		}
		return nil
	}
	if r.Outcome != "rejected" && r.Outcome != "cancelled" {
		return ErrTransition
	}
	if t.Record.RunID != "" || (t.State != "queued" && t.State != "dispatching") {
		return ErrTransition
	}
	if t.Session.ActiveTurnID != "" && t.Session.ActiveTurnID != t.ID {
		return ErrTransition
	}
	return nil
}

func settleSessionOwner(ctx context.Context, tx *sql.Tx, t SessionTurn, outcome string, now time.Time) error {
	state := sessioning.Idle
	var closed any
	if t.Session.State == sessioning.CancelRequested {
		state = sessioning.Closed
		closed = now.UnixNano()
	} else {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM interactive_turns WHERE session_id=? AND state='queued'`, t.Session.ID).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			state = sessioning.Queued
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE interactive_sessions SET state=?,active_turn='',last_outcome=?,updated_ns=?,closed_ns=COALESCE(closed_ns,?),next_sequence=next_sequence+1 WHERE id=?`, state, outcome, now.UnixNano(), closed, t.Session.ID)
	return err
}
