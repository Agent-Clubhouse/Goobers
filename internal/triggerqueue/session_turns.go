package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

// SessionTurn is internal host custody. Authority is never returned in the
// conversation API; the host reconstructs the verified principal for rechecks.
type SessionTurn struct {
	ID        string
	Session   sessioning.Session
	Message   sessioning.Message
	Record    Record
	Authority []byte
	State     string
	Outcome   string
}

// SessionTurn verifies the immutable queue payload against its source ledger.
func (s *Store) SessionTurn(ctx context.Context, acceptance string) (SessionTurn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionTurn{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return readSessionTurn(ctx, tx, acceptance)
}
func readSessionTurn(ctx context.Context, tx *sql.Tx, acceptance string) (SessionTurn, error) {
	var t SessionTurn
	var id, message string
	err := tx.QueryRowContext(ctx, `SELECT id,session_id,message_id,authority,state,outcome FROM interactive_turns WHERE acceptance_id=? AND tombstoned_ns IS NULL`, acceptance).Scan(&t.ID, &id, &message, &t.Authority, &t.State, &t.Outcome)
	if err != nil {
		return t, err
	}
	t.Session, err = scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM interactive_sessions WHERE id=?", id))
	if err != nil {
		return t, err
	}
	t.Message, err = scanSessionMessage(tx.QueryRowContext(ctx, "SELECT "+sessionMessageColumns+" FROM interactive_messages WHERE id=? AND session_id=?", message, id))
	if err != nil {
		return t, err
	}
	t.Record, err = scanRecord(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE id=?", acceptance))
	if err != nil {
		return t, err
	}
	expected := sessioning.StartEnvelope{Kind: sessioning.StartKind, Gaggle: t.Session.Gaggle, SessionID: id, TurnID: t.ID, MessageID: message, MessageDigest: "sha256:" + childDigest([]byte(t.Message.Text)), AuthorityDigest: "sha256:" + childDigest(t.Authority), Profile: t.Session.Profile}
	raw, _ := json.Marshal(expected)
	if string(raw) != string(t.Record.Payload) || t.Message.ActorKind != "human" || t.Message.Actor == nil {
		return SessionTurn{}, errors.New("session turn provenance mismatch")
	}
	return t, nil
}

// BeginSessionTurn is the only dispatch claim for session receipts. It holds
// the per-session FIFO owner and gaggle execution allowance atomically with the
// common queue claim, including across independent Store processes.
func (s *Store) BeginSessionTurn(ctx context.Context, acceptance string, now time.Time) (SessionTurn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionTurn{}, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := readSessionTurn(ctx, tx, acceptance)
	if err != nil {
		return t, err
	}
	if now.IsZero() || t.State != "queued" || t.Record.State != Accepted || t.Session.ActiveTurnID != "" {
		return t, ErrTransition
	}
	if t.Session.State == sessioning.CancelRequested || t.Session.State == sessioning.Closed {
		return t, ErrSessionClosed
	}
	var pinned string
	if err = tx.QueryRowContext(ctx, `SELECT input_digest FROM interactive_turns WHERE id=?`, t.ID).Scan(&pinned); err != nil {
		return t, err
	}
	if pinned == "" {
		return t, ErrTransition
	}
	var ahead, active int
	err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM interactive_turns WHERE session_id=? AND state!='settled' AND sequence<?),(SELECT COUNT(*) FROM interactive_turns WHERE gaggle=? AND state IN ('dispatching','running'))`, t.Session.ID, t.Message.Sequence, t.Session.Gaggle).Scan(&ahead, &active)
	if err != nil {
		return t, err
	}
	if ahead > 0 {
		return t, ErrTransition
	}
	if active >= sessioning.MaxExecutingTurns {
		return t, ErrFull
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_turns SET state='dispatching' WHERE id=?`, t.ID)
	if err != nil {
		return t, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE triggers SET state='dispatching' WHERE id=?`, acceptance)
	if err != nil {
		return t, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_sessions SET state='running',active_turn=?,updated_ns=? WHERE id=?`, t.ID, now.UnixNano(), t.Session.ID)
	if err != nil {
		return t, err
	}
	t, err = readSessionTurn(ctx, tx, acceptance)
	if err != nil {
		return t, err
	}
	return t, tx.Commit()
}

// ObserveSessionRun is called only after verifying the actual published
// journal. Until then the reserved RunID is not exposed as a run link.
func (s *Store) ObserveSessionRun(ctx context.Context, acceptance, runID string, now time.Time) error {
	if runID != strings.TrimPrefix(acceptance, "trigger-") || len(runID) != 32 || now.IsZero() {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := readSessionTurn(ctx, tx, acceptance)
	if err != nil {
		return err
	}
	if t.State == "running" && t.Record.RunID == runID {
		return nil
	}
	if t.State != "dispatching" || t.Session.ActiveTurnID != t.ID || t.Record.State != Dispatching {
		return ErrTransition
	}
	_, err = tx.ExecContext(ctx, `UPDATE triggers SET state='dispatched',run_id=? WHERE id=?`, runID, acceptance)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_turns SET state='running' WHERE id=?`, t.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_messages SET run_id=? WHERE id=?`, runID, t.Message.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RequeueSessionTurn requires host proof of no execution effects (temporary
// admission refusal or strong startup journal absence). Never use it on timeout.
func (s *Store) RequeueSessionTurn(ctx context.Context, acceptance, reason string, now time.Time) error {
	if len(reason) > 1024 || now.IsZero() {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := readSessionTurn(ctx, tx, acceptance)
	if err != nil {
		return err
	}
	if t.State != "dispatching" || t.Record.State != Dispatching || t.Record.RunID != "" || t.Session.ActiveTurnID != t.ID {
		return ErrTransition
	}
	_, err = tx.ExecContext(ctx, `UPDATE triggers SET state='accepted',reason=? WHERE id=?`, reason, acceptance)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_turns SET state='queued' WHERE id=?`, t.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_sessions SET active_turn='',state=CASE WHEN state='cancel-requested' THEN state ELSE 'queued' END,updated_ns=? WHERE id=?`, now.UnixNano(), t.Session.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
