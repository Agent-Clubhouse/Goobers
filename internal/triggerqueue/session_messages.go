package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

// SubmitSessionMessage atomically appends human input and an ordinary queue
// receipt. The caller's verified authority is retained for dispatch rechecks.
func (s *Store) SubmitSessionMessage(ctx context.Context, c SessionCommand, id, text string, authority []byte, now time.Time) (sessioning.Acceptance, error) {
	return s.SubmitSessionInput(ctx, c, id, sessioning.MessageRequest{Text: text}, authority, now)
}

// SubmitSessionInput retains an optional human-selected PR together with the
// exact input digest. Selection grants no provider or execution authority.
func (s *Store) SubmitSessionInput(ctx context.Context, c SessionCommand, id string, input sessioning.MessageRequest, authority []byte, now time.Time) (sessioning.Acceptance, error) {
	input.RepairTarget = sessioning.CopyPRRepairTarget(input.RepairTarget)
	text := input.Text
	target, err := sessioning.MarshalPRRepairTarget(input.RepairTarget)
	if err != nil {
		return sessioning.Acceptance{}, ErrTransition
	}

	if !c.valid() || !validChildText(id, 128, true) || !validSessionText(text) || len(authority) == 0 || len(authority) > 16384 || !json.Valid(authority) || now.IsZero() {
		return sessioning.Acceptance{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if result, found, err := sessionReplay(ctx, tx, c, "message", id); err != nil || found {
		return result, err
	}
	session, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM interactive_sessions WHERE gaggle=? AND id=?", c.Gaggle, id))
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	if session.State == sessioning.Closed || session.State == sessioning.CancelRequested {
		return sessioning.Acceptance{}, ErrSessionClosed
	}
	if err = sessionMessageCapacity(ctx, tx, c.Gaggle, id, len(text)+len(authority)+len(target)); err != nil {
		return sessioning.Acceptance{}, err
	}
	turn := fmt.Sprintf("turn-%x", randomID())
	message := fmt.Sprintf("message-%x", randomID())
	acceptance := fmt.Sprintf("trigger-%x", randomID())
	envelope := sessioning.StartEnvelope{Kind: sessioning.StartKind, Gaggle: c.Gaggle, SessionID: id, TurnID: turn, MessageID: message, MessageDigest: sessioning.MessageDigest(text, input.RepairTarget), AuthorityDigest: "sha256:" + childDigest(authority), Profile: session.Profile}
	payload, _ := json.Marshal(envelope)
	actor, _ := json.Marshal(c.Actor)
	if len(payload) > MaxPayloadBytes {
		return sessioning.Acceptance{}, ErrTransition
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO interactive_messages(id,session_id,sequence,actor_kind,actor,text,created_ns,turn_id,repair_target) VALUES(?,?,?,'human',?,?,?,?,?)`, message, id, session.NextSequence, actor, text, now.UnixNano(), turn, target)
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO interactive_turns(id,session_id,gaggle,message_id,acceptance_id,sequence,authority,state,created_ns,reserved_bytes) VALUES(?,?,?,?,?,?,?,'queued',?,?)`, turn, id, c.Gaggle, message, acceptance, session.NextSequence, authority, now.UnixNano(), sessionResponseAllowance+sessionInputAllowance)
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES(?,?,?,?, 'accepted',?)`, acceptance, "session:"+turn, sessionKey(c, "message", id), payload, now.UnixNano())
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE interactive_sessions SET next_sequence=next_sequence+1,updated_ns=?,state=CASE WHEN active_turn='' THEN 'queued' ELSE state END WHERE id=?`, now.UnixNano(), id)
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	if err = keepSessionRequest(ctx, tx, c, "message", id, id, message, acceptance, now); err != nil {
		return sessioning.Acceptance{}, err
	}
	result, err := sessionAcceptance(ctx, tx, c.Gaggle, id, message, acceptance)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func sessionMessageCapacity(ctx context.Context, tx *sql.Tx, gaggle, id string, size int) error {
	if err := sessionCapacity(ctx, tx, gaggle, 1); err != nil {
		return err
	}
	var queued int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM interactive_turns WHERE session_id=? AND state='queued'`, id).Scan(&queued); err != nil {
		return err
	}
	if queued >= sessioning.MaxQueuedTurns {
		return ErrFull
	}
	if err := triggerSlotCapacity(ctx, tx, 1); err != nil {
		return err
	}
	return childByteCapacity(ctx, tx, size+sessionResponseAllowance+sessionInputAllowance+16*1024)
}

// Session returns one already-authorized gaggle's retained summary.
func (s *Store) Session(ctx context.Context, gaggle, id string) (sessioning.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM interactive_sessions WHERE gaggle=? AND id=?", gaggle, id))
}

// Sessions uses a stable ID cursor. It never scans conversation bodies.
func (s *Store) Sessions(ctx context.Context, gaggle, after string, limit int) (sessioning.SessionPage, error) {
	result := sessioning.SessionPage{Items: []sessioning.Session{}}
	if limit < 1 || limit > sessioning.MaxPageSize || len(after) > 128 {
		return result, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+sessionColumns+" FROM interactive_sessions WHERE gaggle=? AND id>? ORDER BY id LIMIT ?", gaggle, after, limit+1)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		item, err := scanSession(rows)
		if err != nil {
			return result, err
		}
		if len(result.Items) == limit {
			result.NextCursor = result.Items[limit-1].ID
			break
		}
		result.Items = append(result.Items, item)
	}
	return result, rows.Err()
}

// SessionMessages enforces gaggle scope in the same query as the page read.
func (s *Store) SessionMessages(ctx context.Context, gaggle, id string, after uint64, limit int) (sessioning.MessagePage, error) {
	result := sessioning.MessagePage{Items: []sessioning.Message{}}
	if limit < 1 || limit > sessioning.MaxPageSize || after > 1<<62 {
		return result, ErrTransition
	}
	if _, err := s.Session(ctx, gaggle, id); err != nil {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+sessionMessageColumns+` FROM interactive_messages WHERE session_id=? AND sequence>? AND EXISTS(SELECT 1 FROM interactive_sessions WHERE id=? AND gaggle=?) ORDER BY sequence LIMIT ?`, id, after, id, gaggle, limit+1)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	size := 0
	for rows.Next() {
		item, err := scanSessionMessage(rows)
		if err != nil {
			return result, err
		}
		if len(result.Items) == limit || size+sessioning.MessageContentBytes(item) > sessioning.MaxMessagePageBytes {
			result.NextCursor = result.Items[len(result.Items)-1].Sequence
			break
		}
		result.Items = append(result.Items, item)
		size += sessioning.MessageContentBytes(item)
	}
	return result, rows.Err()
}
