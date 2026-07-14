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
	// Retried like the migration loop below: switching journal_mode is
	// itself a lock-acquiring operation, so several processes racing to
	// open the SAME brand-new telemetry.db (e.g. `goobers up` starting at
	// the same instant as a `goobers telemetry` query) can hit SQLITE_BUSY
	// here even with busy_timeout already set on this connection —
	// busy_timeout retries ordinary lock waits, but not every immediate-fail
	// contention this specific pragma can hit during a WAL-mode switch.
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
		if err := execWithBusyRetry(db.sql, migrations[i]); err != nil {
			return fmt.Errorf("rollup: apply migration %d: %w", i+1, err)
		}
	}
	if version < len(migrations) {
		if err := execWithBusyRetry(db.sql, `DELETE FROM schema_meta`); err != nil {
			return fmt.Errorf("rollup: reset schema_meta: %w", err)
		}
		if err := execWithBusyRetry(db.sql, `INSERT INTO schema_meta (version) VALUES (?)`, len(migrations)); err != nil {
			return fmt.Errorf("rollup: record schema version: %w", err)
		}
	}
	return nil
}

// busyRetryMaxAttempts bounds the retry loop execWithBusyRetry uses for
// SQLITE_BUSY/SQLITE_LOCKED during Open() — the only place this package does
// schema-shaped work multiple processes can race on simultaneously (ordinary
// queries elsewhere rely on PRAGMA busy_timeout alone).
const busyRetryMaxAttempts = 5

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
