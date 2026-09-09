// Package triggerqueue holds durable trigger acceptance independently of
// scheduler startup. Dispatching records require reconciliation after a crash;
// they must never be blindly replayed as new scheduler firings.
package triggerqueue

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/sqliteuri"

	_ "modernc.org/sqlite" // Registers the durable ledger driver.
)

// Acceptance limits bound both live custody and retained replay history.
const (
	MaxRecords      = 10000
	MaxPayloadBytes = 16 << 10
	ReplayRetention = 7 * 24 * time.Hour
)

// Admission errors distinguish payload conflicts from exhausted capacity.
var (
	ErrConflict   = errors.New("triggerqueue: key belongs to another request")
	ErrFull       = errors.New("triggerqueue: acceptance capacity exhausted")
	ErrTransition = errors.New("triggerqueue: invalid state transition")
)

// State separates durable acceptance from an observed scheduling outcome.
type State string

// Dispatching survives process death and requires reconciliation before replay.
const (
	Accepted    State = "accepted"
	Dispatching State = "dispatching"
	Dispatched  State = "dispatched"
	Rejected    State = "rejected"
)

// Record preserves the original acceptance time, actor and exact payload.
// Payload must include any server-derived authority needed during dispatch.
type Record struct {
	ID         string
	Key        string
	Actor      string
	Payload    []byte
	State      State
	RunID      string
	Reason     string
	AcceptedAt time.Time
}

// Store serializes transactions while SQLite arbitrates independent processes.
type Store struct{ db *sql.DB }

// Open opens a private database beneath a daemon-owned directory. DELETE
// journaling avoids a WAL that a long reader could retain indefinitely; FULL
// synchronization makes COMMIT the acceptance boundary, not a later checkpoint.
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("triggerqueue: absolute database path required")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		err = file.Close()
	} else if errors.Is(err, os.ErrExist) {
		var info os.FileInfo
		info, err = os.Lstat(path)
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("triggerqueue: database must be a regular file")
		}
	}
	if err != nil {
		return nil, err
	}
	uri := sqliteuri.File(path)
	db, err := sql.Open("sqlite", uri+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)&_pragma=max_page_count(65536)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS triggers (
		id TEXT NOT NULL UNIQUE,
		key TEXT PRIMARY KEY NOT NULL CHECK(length(CAST(key AS BLOB)) BETWEEN 1 AND 256),
		actor TEXT NOT NULL CHECK(length(CAST(actor AS BLOB)) <= 1024),
		payload BLOB NOT NULL CHECK(length(payload) BETWEEN 1 AND 16384),
		state TEXT NOT NULL CHECK(state IN ('accepted','dispatching','dispatched','rejected')),
		run_id TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
		accepted_ns INTEGER NOT NULL, finished_ns INTEGER
	)`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = durability.SyncDir(filepath.Dir(path)); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

const columns = "id,key,actor,payload,state,run_id,reason,accepted_ns"

type scanner interface{ Scan(...any) error }

func scanRecord(row scanner) (Record, error) {
	var r Record
	var ns int64
	err := row.Scan(&r.ID, &r.Key, &r.Actor, &r.Payload, &r.State, &r.RunID, &r.Reason, &ns)
	r.AcceptedAt = time.Unix(0, ns).UTC()
	return r, err
}

// Accept commits the request before returning its stable acceptance ID. Only
// terminal records older than the replay window can be pruned on insert; queued
// or uncertain dispatches retain custody even when the ledger is full.
func (s *Store) Accept(ctx context.Context, key, actor string, payload []byte, now time.Time) (Record, bool, error) {
	if strings.TrimSpace(key) != key || key == "" || len(key) > 256 || len(actor) > 1024 || len(payload) == 0 || len(payload) > MaxPayloadBytes || now.IsZero() {
		return Record{}, false, errors.New("triggerqueue: invalid acceptance")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return Record{}, false, errors.New("triggerqueue: invalid key")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := scanRecord(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE key=?", key))
	if err == nil {
		if r.Actor != actor || string(r.Payload) != string(payload) {
			return Record{}, false, ErrConflict
		}
		return r, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM triggers WHERE finished_ns IS NOT NULL AND finished_ns < ?", now.Add(-ReplayRetention).UnixNano()); err != nil {
		return Record{}, false, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM triggers").Scan(&count); err != nil {
		return Record{}, false, err
	}
	if count >= MaxRecords {
		return Record{}, false, ErrFull
	}
	r = Record{ID: fmt.Sprintf("trigger-%x", randomID()), Key: key, Actor: actor, Payload: append([]byte(nil), payload...), State: Accepted, AcceptedAt: now.UTC()}
	_, err = tx.ExecContext(ctx, "INSERT INTO triggers (id,key,actor,payload,state,accepted_ns) VALUES (?,?,?,?,?,?)", r.ID, r.Key, r.Actor, r.Payload, r.State, now.UnixNano())
	if err != nil {
		return Record{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return r, false, nil
}

func randomID() []byte { b := make([]byte, 16); _, _ = rand.Read(b); return b }

// Get binds lookup to the accepting actor. Unauthorized IDs are indistinguishable
// from missing IDs, including their workflow, payload and dispatch result.
func (s *Store) Get(ctx context.Context, id, actor string) (Record, error) {
	return scanRecord(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE id=? AND actor=?", id, actor))
}

// ForRun is the daemon's internal custody lookup, not an actor-authorized API.
// Assigned run IDs are the random suffix of the durable acceptance ID.
func (s *Store) ForRun(ctx context.Context, runID string) (Record, error) {
	if len(runID) != 32 {
		return Record{}, sql.ErrNoRows
	}
	return scanRecord(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE id=?", "trigger-"+runID))
}

// Pending returns a bounded FIFO batch. Dispatching is deliberately excluded:
// a process may have died after starting the run but before recording its ID.
func (s *Store) Pending(ctx context.Context, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: batch limit must be 1..100")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM triggers WHERE state='accepted' ORDER BY accepted_ns,id LIMIT ?", limit)
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

// BeginDispatch durably claims one request before any scheduler side effect.
// Concurrent workers cannot both claim it, including across Store instances.
func (s *Store) BeginDispatch(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE triggers SET state='dispatching' WHERE id=? AND state='accepted'", id)
	return changed(result, err)
}

// UncertainIDs captures the bounded pre-startup recovery set without loading
// request payloads. Later dispatches must not enter this set: their asynchronous
// starter may still be publishing a journal in the current process.
func (s *Store) UncertainIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM triggers WHERE state='dispatching' LIMIT ?", MaxRecords+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if len(id) != 40 || !strings.HasPrefix(id, "trigger-") || len(ids) >= MaxRecords {
			return nil, errors.New("triggerqueue: invalid recovery snapshot")
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// RetryUnstarted is reserved for restart reconciliation after proving that a
// pre-startup dispatch has no published or staged journal. The acceptance ID
// (and therefore assigned run ID) is unchanged. A concurrent durable receipt
// prevents the transition rather than being overwritten.
func (s *Store) RetryUnstarted(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE triggers SET state='accepted',run_id='' WHERE id=? AND state='dispatching' AND finished_ns IS NULL", id)
	return changed(result, err)
}

// RecordDispatch records scheduler admission, not durable run creation. The
// record stays unfinished and cannot be expired until execution is observed.
func (s *Store) RecordDispatch(ctx context.Context, id, runID string) error {
	if len(runID) > 256 {
		return ErrTransition
	}
	result, err := s.db.ExecContext(ctx, "UPDATE triggers SET run_id=? WHERE id=? AND state='dispatching' AND run_id=''", runID, id)
	return changed(result, err)
}

// Uncertain pages unfinished dispatches by stable ID, including admission whose
// asynchronous starter may not yet have created a durable run. A cursor lets a
// periodic reconciler make progress past unresolved records without starvation.
func (s *Store) Uncertain(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: batch limit must be 1..100")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM triggers WHERE state='dispatching' AND id>? ORDER BY id LIMIT ?", after, limit)
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

// Finish records a definite scheduler outcome. An ambiguous transport outcome
// is not a rejection and must remain Dispatching for reconciliation.
func (s *Store) Finish(ctx context.Context, id string, state State, runID, reason string, now time.Time) error {
	if (state != Dispatched && state != Rejected) || len(runID) > 256 || len(reason) > 1024 || now.IsZero() || (state == Rejected && runID != "") {
		return ErrTransition
	}
	result, err := s.db.ExecContext(ctx, "UPDATE triggers SET state=?,run_id=?,reason=?,finished_ns=? WHERE id=? AND state='dispatching' AND accepted_ns<=?", state, runID, reason, now.UnixNano(), id, now.UnixNano())
	return changed(result, err)
}

func changed(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTransition
	}
	return nil
}
