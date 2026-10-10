package triggerqueue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const sourceStartSchema = `CREATE TABLE source_start_receipts (
 source_key TEXT PRIMARY KEY NOT NULL, actor TEXT NOT NULL, fingerprint TEXT NOT NULL,
 acceptance_ids BLOB NOT NULL, accepted_ns INTEGER NOT NULL
);
`

// MaxSourceStarts bounds the atomic fanout of one scheduler/source observation.
const MaxSourceStarts = 32

// SourceStart is one already pinned workflow start in a trusted source batch.
type SourceStart struct{ Payload []byte }

// SourceAdvance moves a durable schedule cursor in the same transaction as starts.
type SourceAdvance struct {
	Scope         string
	Before, After time.Time
}

// SourceBatch identifies the authenticated delivery independently of recipients.
// A duplicate keeps the first accepted recipient set, even after configuration changes.
type SourceBatch struct {
	Key, Actor, Fingerprint string
	Starts                  []SourceStart
	Advance                 *SourceAdvance
}

// SourceReceipt also records no-match deliveries, preventing later rematching.
type SourceReceipt struct {
	Key, Actor, Fingerprint string
	AcceptanceIDs           []string
	AcceptedAt              time.Time
}

// SourceReceipt reads host custody; this is not a public actor-authorized API.
func (s *Store) SourceReceipt(ctx context.Context, key string) (SourceReceipt, error) {
	return readSourceReceipt(ctx, s.db, key)
}

type sourceQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readSourceReceipt(ctx context.Context, q sourceQuerier, key string) (SourceReceipt, error) {
	var r SourceReceipt
	var raw []byte
	var ns int64
	err := q.QueryRowContext(ctx, `SELECT source_key,actor,fingerprint,acceptance_ids,accepted_ns FROM source_start_receipts WHERE source_key=?`, key).Scan(&r.Key, &r.Actor, &r.Fingerprint, &raw, &ns)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r.AcceptanceIDs); err != nil {
		return r, err
	}
	r.AcceptedAt = time.Unix(0, ns).UTC()
	return r, nil
}

// AcceptSource atomically records every pinned source recipient. Queue
// exhaustion or any invalid recipient rolls the entire batch back.
func (s *Store) AcceptSource(ctx context.Context, b SourceBatch, now time.Time) (SourceReceipt, bool, error) {
	if err := validateSourceBatch(b, now); err != nil {
		return SourceReceipt{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SourceReceipt{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	prior, err := readSourceReceipt(ctx, tx, b.Key)
	if err == nil {
		if prior.Actor != b.Actor || prior.Fingerprint != b.Fingerprint {
			return SourceReceipt{}, false, ErrConflict
		}
		return prior, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SourceReceipt{}, false, err
	}
	if err = prepareSourceBatch(ctx, tx, b, now); err != nil {
		return SourceReceipt{}, false, err
	}
	r := SourceReceipt{Key: b.Key, Actor: b.Actor, Fingerprint: b.Fingerprint, AcceptedAt: now.UTC(), AcceptanceIDs: []string{}}
	for i, start := range b.Starts {
		id := fmt.Sprintf("trigger-%x", randomID())
		key := sourceRecipientKey(b.Key, i)
		if _, err = tx.ExecContext(ctx, `INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES(?,?,?,?,?,?)`, id, key, b.Actor, start.Payload, Accepted, now.UnixNano()); err != nil {
			return SourceReceipt{}, false, err
		}
		r.AcceptanceIDs = append(r.AcceptanceIDs, id)
	}
	raw, err := json.Marshal(r.AcceptanceIDs)
	if err != nil {
		return SourceReceipt{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO source_start_receipts(source_key,actor,fingerprint,acceptance_ids,accepted_ns) VALUES(?,?,?,?,?)`, b.Key, b.Actor, b.Fingerprint, raw, now.UnixNano()); err != nil {
		return SourceReceipt{}, false, err
	}
	if err = advanceSourceCursor(ctx, tx, b.Advance); err != nil {
		return SourceReceipt{}, false, err
	}
	return r, false, tx.Commit()
}

func validateSourceBatch(b SourceBatch, now time.Time) error {
	if !validChildText(b.Key, 256, true) || !validChildText(b.Actor, 1024, true) || !validChildText(b.Fingerprint, 128, true) || now.IsZero() || len(b.Starts) > MaxSourceStarts {
		return errors.New("triggerqueue: invalid source batch")
	}
	for _, start := range b.Starts {
		if len(start.Payload) == 0 || len(start.Payload) > MaxPayloadBytes {
			return errors.New("triggerqueue: invalid source start")
		}
	}
	if a := b.Advance; a != nil && (!validChildText(a.Scope, 256, true) || a.Before.IsZero() || !a.After.After(a.Before)) {
		return errors.New("triggerqueue: invalid source advance")
	}
	return nil
}
func prepareSourceBatch(ctx context.Context, tx *sql.Tx, b SourceBatch, now time.Time) error {
	// Source receipts have the same documented replay horizon as ordinary keys.
	if _, err := tx.ExecContext(ctx, `DELETE FROM source_start_receipts WHERE accepted_ns<? AND NOT EXISTS (SELECT 1 FROM json_each(acceptance_ids) ids JOIN triggers t ON t.id=ids.value WHERE t.finished_ns IS NULL)`, now.Add(-ReplayRetention).UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM triggers WHERE finished_ns IS NOT NULL AND finished_ns<?`, now.Add(-ReplayRetention).UnixNano()); err != nil {
		return err
	}
	if err := triggerSlotCapacity(ctx, tx, len(b.Starts)+1); err != nil {
		return err
	}
	size := len(b.Key) + len(b.Actor) + len(b.Fingerprint) + 4096
	for _, start := range b.Starts {
		size += len(start.Payload) + len(b.Actor) + 4096
	}
	return childByteCapacity(ctx, tx, size)
}
func sourceRecipientKey(key string, ordinal int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", key, ordinal)))
	return "source-start:" + hex.EncodeToString(digest[:])
}

// Shared receipt pressure includes cursors and empty deliveries as well as starts.
func triggerSlotCapacity(ctx context.Context, tx *sql.Tx, additional int) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM triggers)+(SELECT COUNT(*) FROM source_start_receipts)+(SELECT COUNT(*) FROM source_start_cursors)").Scan(&count); err != nil {
		return err
	}
	if additional < 0 || count > MaxRecords-additional {
		return ErrFull
	}
	return nil
}
