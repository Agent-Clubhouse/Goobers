package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxDirectEngineInputBytes bounds the canonical, credential-free execution input.
const MaxDirectEngineInputBytes = 1 << 20

const directEngineSchema = `CREATE TABLE direct_engine_inputs (
 acceptance_id TEXT PRIMARY KEY NOT NULL,
 input BLOB NOT NULL CHECK(length(input) BETWEEN 1 AND 1048576)
);
CREATE TRIGGER direct_engine_input_delete AFTER DELETE ON triggers BEGIN
 DELETE FROM direct_engine_inputs WHERE acceptance_id=OLD.id;
END;`

// AcceptDirectEngine atomically gives an ordinary receipt custody of its exact
// engine input. The attachment consumes the same database byte and slot limits.
// The host must validate the closed descriptor and credential-free input first.
func (s *Store) AcceptDirectEngine(ctx context.Context, key, actor string, payload, input []byte, now time.Time) (Record, bool, error) {
	if err := validateDirectEngineAcceptance(key, actor, payload, input, now); err != nil {
		return Record{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	prior, err := scanRecord(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE key=?", key))
	if err == nil {
		var saved []byte
		readErr := tx.QueryRowContext(ctx, "SELECT input FROM direct_engine_inputs WHERE acceptance_id=?", prior.ID).Scan(&saved)
		if readErr != nil || prior.Actor != actor || string(prior.Payload) != string(payload) || string(saved) != string(input) {
			return Record{}, false, ErrConflict
		}
		return prior, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM triggers WHERE finished_ns IS NOT NULL AND finished_ns < ? AND NOT EXISTS(SELECT 1 FROM interactive_turns st WHERE st.acceptance_id=triggers.id)", now.Add(-ReplayRetention).UnixNano()); err != nil {
		return Record{}, false, err
	}
	if err = triggerSlotCapacity(ctx, tx, 1); err != nil {
		return Record{}, false, err
	}
	if err = childByteCapacity(ctx, tx, len(input)+len(payload)+len(key)+len(actor)); err != nil {
		return Record{}, false, err
	}
	record := Record{ID: fmt.Sprintf("trigger-%x", randomID()), Key: key, Actor: actor, Payload: append([]byte(nil), payload...), State: Accepted, AcceptedAt: now.UTC()}
	if _, err = tx.ExecContext(ctx, "INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES(?,?,?,?,?,?)", record.ID, key, actor, payload, Accepted, now.UnixNano()); err != nil {
		return Record{}, false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO direct_engine_inputs(acceptance_id,input) VALUES(?,?)", record.ID, input); err != nil {
		return Record{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return record, false, nil
}

// DirectEngineInput reads host-owned immutable input for a direct receipt.
// It is not an actor-authorized user API.
func (s *Store) DirectEngineInput(ctx context.Context, id string) ([]byte, error) {
	var input []byte
	err := s.db.QueryRowContext(ctx, "SELECT input FROM direct_engine_inputs WHERE acceptance_id=?", id).Scan(&input)
	return input, err
}

// DirectEnginePage gives remote attempts an independent bounded cursor so an
// unavailable frontend cannot monopolize ordinary pending or uncertain batches.
func (s *Store) DirectEnginePage(ctx context.Context, after string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM triggers WHERE id>? AND state IN ('accepted','dispatching') AND EXISTS(SELECT 1 FROM direct_engine_inputs i WHERE i.acceptance_id=triggers.id) ORDER BY id LIMIT 1", after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func validateDirectEngineAcceptance(key, actor string, payload, input []byte, now time.Time) error {
	if key == "" || strings.TrimSpace(key) != key || len(key) > 256 || actor == "" || len(actor) > 1024 || len(payload) == 0 || len(payload) > MaxPayloadBytes || len(input) == 0 || len(input) > MaxDirectEngineInputBytes || now.IsZero() {
		return errors.New("triggerqueue: invalid direct engine acceptance")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return errors.New("triggerqueue: invalid direct engine key")
		}
	}
	return nil
}
