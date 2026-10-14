package triggerqueue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const scheduleDemandSchema = `CREATE TABLE schedule_demands (
 scope TEXT PRIMARY KEY NOT NULL, id TEXT UNIQUE NOT NULL, payload BLOB NOT NULL,
 observed_count INTEGER NOT NULL DEFAULT -1, queued INTEGER NOT NULL DEFAULT 0,
 accepted_ns INTEGER NOT NULL
);`

// ScheduleDemandCapture pins a nominal fire before provider sizing. Independent
// captures retain every occurrence; the zero value preserves coalesced custody.
// This is a host-owned storage contract, not a public workflow policy.
type ScheduleDemandCapture struct {
	ID          string
	Advance     SourceAdvance
	Payload     []byte
	Independent bool
	AcceptedAt  time.Time
}

// ScheduleDemand retains one pinned fire before and after sizing.
// Count -1 requires a provider observation. Queued ordinals never repeat.
type ScheduleDemand struct {
	ID, Scope     string
	Payload       []byte
	Count, Queued int
	AcceptedAt    time.Time
	Independent   bool
	fingerprint   string
}

// DemandTransfer moves a consecutive interval from an obligation into starts.
type DemandTransfer struct {
	ID            string
	Before, After int
}

// CaptureScheduleDemand captures pins before sizing and advances the cursor in
// the same transaction. By default, later fires coalesce without replacing pins
// or observed counts. Independent captures retain one obligation per occurrence.
func (s *Store) CaptureScheduleDemand(ctx context.Context, capture ScheduleDemandCapture) error {
	id, advance, payload, now := capture.ID, capture.Advance, capture.Payload, capture.AcceptedAt
	if !validChildText(id, 256, true) || len(payload) == 0 || len(payload) > MaxPayloadBytes || now.IsZero() {
		return errors.New("triggerqueue: invalid schedule demand")
	}
	if err := validateSourceBatch(SourceBatch{Key: id, Actor: "scheduler", Fingerprint: id, Advance: &advance}, now); err != nil {
		return err
	}
	fingerprint, err := scheduleDemandFingerprint(capture)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	prior, err := readScheduleDemand(ctx, tx, "SELECT "+scheduleDemandColumns+" FROM schedule_demands WHERE id=?", id)
	if err == nil {
		if prior.fingerprint != fingerprint {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var found int
	if !capture.Independent {
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schedule_demands WHERE scope=? AND independent=0`, advance.Scope).Scan(&found); err != nil {
			return err
		}
	}
	if found == 0 {
		if err = triggerSlotCapacity(ctx, tx, 1); err != nil {
			return err
		}
		if err = childByteCapacity(ctx, tx, len(payload)+len(id)+len(advance.Scope)+len(fingerprint)+4096); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schedule_demands(scope,id,payload,accepted_ns,independent,fingerprint) VALUES(?,?,?,?,?,?)`, advance.Scope, id, payload, now.UnixNano(), capture.Independent, fingerprint); err != nil {
			return err
		}
	}
	if err = advanceSourceCursor(ctx, tx, &advance); err != nil {
		return err
	}
	return tx.Commit()
}

func scheduleDemandFingerprint(capture ScheduleDemandCapture) (string, error) {
	// Retry time is not identity. Normalize cursor times so location and monotonic
	// clock metadata cannot make a retry differ from its original capture.
	capture.AcceptedAt = time.Time{}
	capture.Advance.Before = capture.Advance.Before.UTC()
	capture.Advance.After = capture.Advance.After.UTC()
	raw, err := json.Marshal(capture)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

const scheduleDemandColumns = "id,scope,payload,observed_count,queued,accepted_ns,independent,fingerprint"

// ScheduleDemand reads the oldest outstanding obligation for a configured scope.
func (s *Store) ScheduleDemand(ctx context.Context, scope string) (ScheduleDemand, error) {
	return readScheduleDemand(ctx, s.db, "SELECT "+scheduleDemandColumns+" FROM schedule_demands WHERE scope=? ORDER BY accepted_ns,id LIMIT 1", scope)
}

// ScheduleDemandByID reads the exact retained occurrence during worker transfer.
func (s *Store) ScheduleDemandByID(ctx context.Context, id string) (ScheduleDemand, error) {
	return readScheduleDemand(ctx, s.db, "SELECT "+scheduleDemandColumns+" FROM schedule_demands WHERE id=?", id)
}

func readScheduleDemand(ctx context.Context, q sourceQuerier, query string, arg any) (ScheduleDemand, error) {
	var result ScheduleDemand
	var ns int64
	err := q.QueryRowContext(ctx, query, arg).Scan(&result.ID, &result.Scope, &result.Payload, &result.Count, &result.Queued, &ns, &result.Independent, &result.fingerprint)
	if err == nil && (result.Count < -1 || result.Count > MaxRecords || result.Queued < 0 || (result.Count < 0 && result.Queued != 0) || (result.Count >= 0 && result.Queued > result.Count)) {
		return result, errors.New("triggerqueue: corrupt schedule demand progress")
	}
	result.AcceptedAt = time.Unix(0, ns).UTC()
	return result, err
}

// ObserveScheduleDemand seals a bounded count once. Zero closes the fire; failed
// or quota-shed observations are not passed here and preserve the obligation.
func (s *Store) ObserveScheduleDemand(ctx context.Context, id string, count int) error {
	if count < 0 || count > MaxRecords {
		return errors.New("triggerqueue: schedule observation exceeds bounded worker count")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	prior, err := readScheduleDemand(ctx, tx, "SELECT "+scheduleDemandColumns+" FROM schedule_demands WHERE id=?", id)
	if err != nil {
		return err
	}
	if prior.Count >= 0 {
		if prior.Count != count {
			return ErrConflict
		}
		return nil
	}
	if count == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM schedule_demands WHERE id=?`, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE schedule_demands SET observed_count=? WHERE id=?`, count, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func transferScheduleDemand(ctx context.Context, tx *sql.Tx, b SourceBatch) error {
	d := b.Demand
	if d == nil {
		return nil
	}
	if d.Before < 0 || d.After <= d.Before || d.After-d.Before != len(b.Starts) {
		return errors.New("triggerqueue: invalid demand transfer")
	}
	result, err := tx.ExecContext(ctx, `UPDATE schedule_demands SET queued=? WHERE id=? AND queued=? AND observed_count>=?`, d.After, d.ID, d.Before, d.After)
	if err = changed(result, err); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM schedule_demands WHERE id=? AND queued=observed_count`, d.ID)
	return err
}

// ScheduleDemandPage retains archived definitions until all starts own pins.
func (s *Store) ScheduleDemandPage(ctx context.Context, after string, limit int) ([]ScheduleDemand, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid schedule demand page")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+scheduleDemandColumns+" FROM schedule_demands WHERE id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []ScheduleDemand
	for rows.Next() {
		var d ScheduleDemand
		var ns int64
		if err = rows.Scan(&d.ID, &d.Scope, &d.Payload, &d.Count, &d.Queued, &ns, &d.Independent, &d.fingerprint); err != nil {
			return nil, err
		}
		d.AcceptedAt = time.Unix(0, ns).UTC()
		result = append(result, d)
	}
	return result, rows.Err()
}
