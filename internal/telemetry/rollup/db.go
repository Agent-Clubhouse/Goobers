package rollup

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite" // pure-Go driver: no cgo, matches the local-runner's
	// single-binary/no-service-dependency posture (ARCHITECTURE.md §3.1).
)

// DB is an open handle to the local telemetry rollup (telemetry.db, TEL-032).
// It is derived state, always rebuildable from the run journals via Rebuild —
// never treat it as a source of truth.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if needed) the SQLite rollup at path and applies any
// pending forward migrations (seeds the V1 upgrade story, #33). WAL mode lets
// a reader (a `goobers telemetry`/`goobers trace` query) proceed concurrently
// with a writer (the daemon's incremental ingest on run finish, #127)
// instead of blocking behind SQLITE_BUSY; busy_timeout covers the remaining
// writer-vs-writer window (two CLI invocations racing an explicit --rebuild)
// by retrying instead of failing immediately.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("rollup: open %s: %w", path, err)
	}
	// A single connection avoids SQLite "database is locked" errors from our
	// own concurrent writers; the rollup is a local, single-process store.
	sqlDB.SetMaxOpenConns(1)
	// busy_timeout MUST be set before anything else touches the file: a
	// concurrent process creating the schema (or holding the WAL init lock)
	// can make even the "PRAGMA journal_mode=WAL" statement itself return
	// SQLITE_BUSY immediately, with no retry, if the connection's own
	// busy-wait isn't already configured.
	if err := execWithBusyRetry(sqlDB, `PRAGMA busy_timeout=5000`); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("rollup: set busy_timeout on %s: %w", path, err)
	}
	// Retried like applyMigration below: switching journal_mode is itself a
	// lock-acquiring operation, so several processes racing to open the SAME
	// brand-new telemetry.db (e.g. `goobers up` starting at the same instant
	// as a `goobers telemetry` query) can hit SQLITE_BUSY here even with
	// busy_timeout already set on this connection — busy_timeout retries
	// ordinary lock waits, but not every immediate-fail contention this
	// specific pragma can hit during a WAL-mode switch.
	if err := execWithBusyRetry(sqlDB, `PRAGMA journal_mode=WAL`); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("rollup: set WAL mode on %s: %w", path, err)
	}
	db := &DB{sql: sqlDB}
	if err := db.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close closes the underlying database handle.
func (db *DB) Close() error { return db.sql.Close() }

func (db *DB) migrate() error {
	if err := execWithBusyRetry(db.sql, `CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("rollup: create schema_meta: %w", err)
	}
	version, err := db.schemaVersion()
	if err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		if err := db.applyMigration(i); err != nil {
			return err
		}
	}
	return nil
}

// busyRetryMaxAttempts bounds the retry loop execWithBusyRetry/applyMigration
// use for SQLITE_BUSY/SQLITE_LOCKED during Open() — the only place this
// package does schema-shaped work multiple processes can race on
// simultaneously (ordinary queries elsewhere rely on PRAGMA busy_timeout
// alone). SQLITE_BUSY_SNAPSHOT (the case this retry actually exists for,
// since busy_timeout's own C-level busy-handler does NOT retry it — a stale
// read snapshot can't be waited out, only abandoned and retaken) can persist
// for multiple retry rounds under N-way concurrent first-Opens of the same
// fresh file; 5 attempts at up to 100ms backoff (300ms total) still flaked
// ~1/30 under an 8-goroutine stress test wrapping a whole migration
// transaction, so this is sized with real headroom rather than the tightest
// value that happened to pass a few runs.
const busyRetryMaxAttempts = 12

// execWithBusyRetry runs a single statement, retrying on SQLITE_BUSY/LOCKED
// with a short linear backoff. busy_timeout alone doesn't cover every
// contention shape Open() can hit: a fresh telemetry.db with two
// near-simultaneous first Opens (e.g. `goobers up` racing a `goobers
// telemetry` query at instance startup) can make even a single statement —
// the WAL-mode pragma, or one migration statement — return SQLITE_BUSY
// immediately rather than blocking, if the busy handler doesn't cover that
// specific lock-upgrade shape.
func execWithBusyRetry(sqlDB *sql.DB, query string, args ...any) error {
	var err error
	for attempt := 1; attempt <= busyRetryMaxAttempts; attempt++ {
		if _, err = sqlDB.Exec(query, args...); err == nil || !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
	}
	return err
}

// applyMigration runs migrations[i] and records the resulting schema version
// in ONE transaction, so a crash between the two can never leave them out of
// sync. Every migration so far is CREATE TABLE/INDEX IF NOT EXISTS (safe to
// blindly re-run), but that won't always be true — a future migration that
// needs ALTER TABLE is not idempotent, and without this transactional
// pairing, a crash after applying it but before recording the version bump
// would re-apply (and fail on) the same ALTER TABLE on the next restart
// (issue #129). SQLite supports transactional DDL, so this is safe here
// specifically — this package only ever targets SQLite (modernc.org/sqlite).
//
// Retried on SQLITE_BUSY (same rationale as execWithBusyRetry above): a
// fresh telemetry.db with two near-simultaneous first Opens (e.g. `goobers
// up` racing a `goobers telemetry` query at instance startup) can hit WAL's
// write-lock-upgrade contention here even with busy_timeout set — that
// PRAGMA covers ordinary lock waits, but a transaction that started as a
// read and tries to upgrade to a writer while another connection already
// holds the write lock can still surface SQLITE_BUSY immediately rather
// than blocking.
func (db *DB) applyMigration(i int) error {
	var err error
	for attempt := 1; attempt <= busyRetryMaxAttempts; attempt++ {
		if err = db.applyMigrationOnce(i); err == nil || !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
	}
	return err
}

func (db *DB) applyMigrationOnce(i int) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("rollup: begin migration %d: %w", i+1, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	if _, err := tx.Exec(migrations[i]); err != nil {
		return fmt.Errorf("rollup: apply migration %d: %w", i+1, err)
	}
	if _, err := tx.Exec(`DELETE FROM schema_meta`); err != nil {
		return fmt.Errorf("rollup: reset schema_meta after migration %d: %w", i+1, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_meta (version) VALUES (?)`, i+1); err != nil {
		return fmt.Errorf("rollup: record schema version %d: %w", i+1, err)
	}
	return tx.Commit()
}

// isSQLiteBusy reports whether err (or a wrapped cause) is SQLITE_BUSY (5) or
// SQLITE_LOCKED (6) — the two primary codes a bounded retry can resolve.
// modernc.org/sqlite's Error.Code() returns the EXTENDED result code (e.g.
// 517 = SQLITE_BUSY_SNAPSHOT = 5 | (2<<8)), not the primary one, so the
// primary code must be masked out with &0xFF before comparing — comparing
// the raw extended code against 5/6 misses every extended-busy variant.
func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xFF
	return code == 5 || code == 6 // SQLITE_BUSY, SQLITE_LOCKED
}

func (db *DB) schemaVersion() (int, error) {
	var version int
	err := db.sql.QueryRow(`SELECT version FROM schema_meta LIMIT 1`).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("rollup: read schema version: %w", err)
	}
	return version, nil
}
