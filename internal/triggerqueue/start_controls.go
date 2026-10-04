package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const startControlAllowance = 24 << 10

const startControlSchema = `CREATE TABLE start_controls (
 acceptance_id TEXT PRIMARY KEY NOT NULL,
 gaggle TEXT NOT NULL DEFAULT '', scope BLOB NOT NULL DEFAULT x'',
 deadline_ns INTEGER, cancellation BLOB NOT NULL DEFAULT x'',
 cancel_ns INTEGER, disposition TEXT NOT NULL DEFAULT '', disposed_ns INTEGER,
 reserved_bytes INTEGER NOT NULL DEFAULT 24576 CHECK(reserved_bytes>=0)
);
INSERT INTO start_controls(acceptance_id) SELECT id FROM triggers;
CREATE TRIGGER start_control_insert AFTER INSERT ON triggers BEGIN
 INSERT INTO start_controls(acceptance_id) VALUES(NEW.id);
END;
CREATE TRIGGER start_control_delete AFTER DELETE ON triggers BEGIN
 DELETE FROM start_controls WHERE acceptance_id=OLD.id;
END;
CREATE INDEX start_control_gaggle ON start_controls(gaggle,acceptance_id);
UPDATE event_receipts SET reserved_bytes=reserved_bytes+24576*reserved_starts WHERE state='routing_pending';
UPDATE event_groups SET reserved_bytes=reserved_bytes+24576*reserved_starts WHERE state='open';`

// ErrTypedStartSettlement refuses missing, mismatched or attempted typed custody.
var ErrTypedStartSettlement = errors.New("triggerqueue: typed start custody cannot be settled")

// StartScope is derived by a trusted host from validated accepted bytes and the
// exact retained generation. No field is accepted from a cancellation body.
type StartScope struct {
	Gaggle, Workflow, Kind, Source, Generation, PayloadDigest, ReservedRunID string
	Deadline                                                                 time.Time
}

// StartCancellation retains verified human authority separately from client data.
// Authority is bounded host-produced JSON and must contain no credential values.
type StartCancellation struct {
	RequestID, Actor, Reason string
	Authority                []byte
}

// StartControl preserves durable queue disposition independently of run state.
// A cancellation request does not establish that an attempted execution stopped.
type StartControl struct {
	CancellationOutcome    string
	CancellationObservedAt time.Time
	Record                 Record
	Scope                  StartScope
	Cancellation           *StartCancellation
	CancelRequestedAt      time.Time
	Disposition            string
	DisposedAt             time.Time
}

func (s StartScope) valid(record Record) bool {
	for _, value := range []string{s.Gaggle, s.Workflow, s.Kind, s.Source, s.Generation, s.ReservedRunID} {
		if !validChildText(value, 256, true) {
			return false
		}
	}
	if !validSessionDigest(s.PayloadDigest) || s.PayloadDigest != "sha256:"+childDigest(record.Payload) {
		return false
	}
	if !s.Deadline.IsZero() && s.Deadline.Before(record.AcceptedAt) {
		return false
	}
	switch s.Source {
	case "manual", "schedule", "backlog", "event", "child", "session", "human-restart", "direct-engine", "signal", "legacy":
		return true
	default:
		return false
	}
}

// PinStartControl binds immutable scope and deadline once. Concurrent resolvers
// must agree exactly; current mutable configuration cannot retarget a receipt.
func (s *Store) PinStartControl(ctx context.Context, id string, scope StartScope) (StartControl, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StartControl{}, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := scanRecord(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE id=?", id))
	if err != nil {
		return StartControl{}, err
	}
	if !scope.valid(record) {
		return StartControl{}, ErrTransition
	}
	raw, err := json.Marshal(scope)
	if err != nil || len(raw) > 4096 {
		return StartControl{}, ErrTransition
	}
	var prior []byte
	if err = tx.QueryRowContext(ctx, `SELECT scope FROM start_controls WHERE acceptance_id=?`, id).Scan(&prior); err != nil {
		return StartControl{}, err
	}
	if len(prior) != 0 && string(prior) != string(raw) {
		return StartControl{}, ErrConflict
	}
	if len(prior) == 0 {
		var deadline any
		if !scope.Deadline.IsZero() {
			deadline = scope.Deadline.UnixNano()
		}
		_, err = tx.ExecContext(ctx, `UPDATE start_controls SET gaggle=?,scope=?,deadline_ns=?,reserved_bytes=reserved_bytes-? WHERE acceptance_id=? AND length(scope)=0`, scope.Gaggle, raw, deadline, len(raw)+1024, id)
		if err != nil {
			return StartControl{}, err
		}
	}
	result, err := readStartControl(ctx, tx, scope.Gaggle, id)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// StartControl reads only within an already authorized gaggle. It does not
// authorize the caller and deliberately includes internal source bytes.
func (s *Store) StartControl(ctx context.Context, gaggle, id string) (StartControl, error) {
	return readStartControl(ctx, s.db, gaggle, id)
}

func readStartControl(ctx context.Context, q sourceQuerier, gaggle, id string) (StartControl, error) {
	var c StartControl
	c.Record.ID = id
	var scope, cancel []byte
	var deadline, cancelNS, disposed, observed sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT scope,deadline_ns,cancellation,cancel_ns,disposition,disposed_ns,cancel_outcome,cancel_observed_ns FROM start_controls WHERE gaggle=? AND acceptance_id=? AND length(scope)>0`, gaggle, id).Scan(&scope, &deadline, &cancel, &cancelNS, &c.Disposition, &disposed, &c.CancellationOutcome, &observed)
	if err != nil {
		return c, err
	}
	c.Record, err = scanRecord(q.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE id=?", id))
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(scope, &c.Scope); err != nil || !c.Scope.valid(c.Record) || c.Scope.Gaggle != gaggle {
		return c, ErrConflict
	}
	if deadline.Valid != !c.Scope.Deadline.IsZero() || (deadline.Valid && deadline.Int64 != c.Scope.Deadline.UnixNano()) {
		return c, ErrConflict
	}
	if len(cancel) > 0 {
		c.Cancellation = new(StartCancellation)
		if err = json.Unmarshal(cancel, c.Cancellation); err != nil || !c.Cancellation.valid() {
			return c, ErrConflict
		}
	}
	if cancelNS.Valid {
		c.CancelRequestedAt = time.Unix(0, cancelNS.Int64).UTC()
	}
	if disposed.Valid {
		c.DisposedAt = time.Unix(0, disposed.Int64).UTC()
	}
	if observed.Valid {
		c.CancellationObservedAt = time.Unix(0, observed.Int64).UTC()
	}
	if !c.validDisposition() || !c.validCancellationOutcome() {
		return c, ErrConflict
	}
	return c, nil
}

func (c StartControl) validDisposition() bool {
	if (c.Cancellation != nil) != !c.CancelRequestedAt.IsZero() || (!c.CancelRequestedAt.IsZero() && c.CancelRequestedAt.Before(c.Record.AcceptedAt)) {
		return false
	}
	if c.Disposition == "" {
		return c.DisposedAt.IsZero()
	}
	if c.Record.State != Rejected || c.DisposedAt.IsZero() || c.DisposedAt.Before(c.Record.AcceptedAt) {
		return false
	}
	switch c.Disposition {
	case "cancelled":
		return c.Cancellation != nil
	case "expired":
		return c.Cancellation == nil && !c.Scope.Deadline.IsZero() && !c.DisposedAt.Before(c.Scope.Deadline)
	default:
		return false
	}
}

// UnpinnedStartControls bounds migration/inventory work without interpreting
// arbitrary legacy bytes as authority. Only a host validator may pin them.
func (s *Store) UnpinnedStartControls(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 || len(after) > 128 {
		return nil, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+` FROM triggers JOIN start_controls c ON c.acceptance_id=id WHERE length(c.scope)=0 AND id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// StartControlPage exposes bounded per-gaggle custody without raw cancellation
// authority reaching a transport. Transport DTOs must select safe fields only.
func (s *Store) StartControlPage(ctx context.Context, gaggle, after string, limit int) ([]StartControl, error) {
	if !validChildText(gaggle, 256, true) || len(after) > 128 || limit < 1 || limit > 100 {
		return nil, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, `SELECT acceptance_id FROM start_controls WHERE gaggle=? AND acceptance_id>? AND length(scope)>0 ORDER BY acceptance_id LIMIT ?`, gaggle, after, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	result := make([]StartControl, 0, len(ids))
	for _, id := range ids {
		c, err := s.StartControl(ctx, gaggle, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, nil
}

// PinnedStartControl is an internal host lookup. User-facing callers must first
// authorize the returned gaggle; no actor/source claims come from wire bodies.
func (s *Store) PinnedStartControl(ctx context.Context, id string) (StartControl, error) {
	var gaggle string
	if err := s.db.QueryRowContext(ctx, `SELECT gaggle FROM start_controls WHERE acceptance_id=? AND length(scope)>0`, id).Scan(&gaggle); err != nil {
		return StartControl{}, err
	}
	return s.StartControl(ctx, gaggle, id)
}

// StartControlInventory bounds scope indexing and pending maintenance. Terminal
// controls with a recorded disposition do not consume repeated sweep work.
func (s *Store) StartControlInventory(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 || len(after) > 128 {
		return nil, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+` FROM triggers JOIN start_controls c ON c.acceptance_id=id WHERE id>? AND (length(c.scope)=0 OR state='accepted' OR (c.cancel_ns IS NOT NULL AND c.disposition='' AND c.cancel_outcome='')) ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (c StartControl) validCancellationOutcome() bool {
	if c.CancellationOutcome == "" {
		return c.CancellationObservedAt.IsZero()
	}
	return c.Cancellation != nil && !c.CancellationObservedAt.IsZero() && !c.CancellationObservedAt.Before(c.CancelRequestedAt) && c.Disposition == "" && (c.CancellationOutcome == "confirmed" || c.CancellationOutcome == "already-terminal")
}
