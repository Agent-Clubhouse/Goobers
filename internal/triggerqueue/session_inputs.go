package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/sessioning"
)

const sessionInputAllowance = sessioning.MaxContextBytes + 16*1024
const sessionInputSchema = `
ALTER TABLE interactive_turns ADD COLUMN inputs BLOB NOT NULL DEFAULT '' CHECK(length(inputs)<=262144);
ALTER TABLE interactive_turns ADD COLUMN input_digest TEXT NOT NULL DEFAULT '';
UPDATE interactive_turns SET reserved_bytes=reserved_bytes+278528 WHERE state!='settled';
`

// SessionInputs retains one bounded reconstruction before dispatch. Earlier
// settled responses are ordered with their originating turn, even when humans
// queued their next messages before those responses arrived. Later input is
// excluded. Exact bytes survive replay and cannot be rebuilt with new context.
func (s *Store) SessionInputs(ctx context.Context, acceptance string) (sessioning.ExecutionInputs, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessioning.ExecutionInputs{}, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := readSessionTurn(ctx, tx, acceptance)
	if err != nil {
		return sessioning.ExecutionInputs{}, err
	}
	var raw []byte
	var digest string
	if err = tx.QueryRowContext(ctx, `SELECT inputs,input_digest FROM interactive_turns WHERE id=?`, t.ID).Scan(&raw, &digest); err != nil {
		return sessioning.ExecutionInputs{}, err
	}
	runID := strings.TrimPrefix(acceptance, "trigger-")
	if len(raw) > 0 {
		if digest != sessioning.Digest(raw) {
			return sessioning.ExecutionInputs{}, errors.New("session input custody digest mismatch")
		}
		in, err := sessioning.ParseExecutionInputs(raw, runID, t.Session.Gaggle)
		if err != nil {
			return in, err
		}
		start, _ := json.Marshal(in.Start)
		if string(start) != string(t.Record.Payload) || in.AcceptanceID != acceptance {
			return in, errors.New("session input differs from queue custody")
		}
		return in, nil
	}
	if t.State != "queued" || t.Record.State != Accepted {
		return sessioning.ExecutionInputs{}, ErrTransition
	}
	var ahead int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM interactive_turns WHERE session_id=? AND state!='settled' AND sequence<?`, t.Session.ID, t.Message.Sequence).Scan(&ahead); err != nil {
		return sessioning.ExecutionInputs{}, err
	}
	if ahead > 0 || t.Session.ActiveTurnID != "" {
		return sessioning.ExecutionInputs{}, ErrTransition
	}
	if t.Session.State == sessioning.CancelRequested || t.Session.State == sessioning.Closed {
		return sessioning.ExecutionInputs{}, ErrSessionClosed
	}
	in, err := reconstructSessionInput(ctx, tx, t)
	if err != nil {
		return in, err
	}
	raw, err = in.Validate(runID, t.Session.Gaggle)
	if err != nil {
		return in, err
	}
	if err = childByteCapacity(ctx, tx, len(raw)+4096-sessionInputAllowance); err != nil {
		return in, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE interactive_turns SET inputs=?,input_digest=?,reserved_bytes=reserved_bytes-? WHERE id=? AND input_digest='' AND reserved_bytes>=?`, raw, sessioning.Digest(raw), sessionInputAllowance, t.ID, sessionInputAllowance)
	if err = changed(res, err); err != nil {
		return in, err
	}
	return in, tx.Commit()
}

func reconstructSessionInput(ctx context.Context, tx *sql.Tx, t SessionTurn) (sessioning.ExecutionInputs, error) {
	var envelope sessioning.StartEnvelope
	if err := json.Unmarshal(t.Record.Payload, &envelope); err != nil {
		return sessioning.ExecutionInputs{}, err
	}
	in := sessioning.ExecutionInputs{Version: 1, AcceptanceID: t.Record.ID, Start: envelope, Messages: []sessioning.Message{t.Message}}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.session_id,m.sequence,m.actor_kind,m.actor,m.text,m.created_ns,m.turn_id,m.run_id,m.outcome,m.repair_target FROM interactive_messages m JOIN interactive_turns t ON t.id=m.turn_id WHERE t.session_id=? AND t.state='settled' AND t.sequence<? ORDER BY t.sequence DESC,m.sequence DESC LIMIT ?`, t.Session.ID, t.Message.Sequence, sessioning.MaxContextMessages)
	if err != nil {
		return in, err
	}
	defer func() { _ = rows.Close() }()
	// Collect newest-first while reserving the current message and all manifest
	// metadata. A failed size check excludes the entire older message.
	var previous []sessioning.Message
	for rows.Next() {
		m, err := scanSessionMessage(rows)
		if err != nil {
			return in, err
		}
		candidate := append([]sessioning.Message(nil), previous...)
		candidate = append(candidate, m)
		slices.Reverse(candidate)
		candidate = append(candidate, t.Message)
		trial := in
		trial.Messages = candidate
		if _, err = trial.Validate(strings.TrimPrefix(t.Record.ID, "trigger-"), t.Session.Gaggle); err != nil {
			in.Truncated = true
			break
		}
		previous = append(previous, m)
	}
	if err = rows.Err(); err != nil {
		return in, err
	}
	slices.Reverse(previous)
	in.Messages = append(previous, t.Message)
	return in, nil
}
