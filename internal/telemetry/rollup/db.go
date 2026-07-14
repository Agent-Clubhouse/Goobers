package rollup

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, matches the local-runner's
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
	if _, err := sqlDB.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("rollup: set busy_timeout on %s: %w", path, err)
	}
	if _, err := sqlDB.Exec(`PRAGMA journal_mode=WAL`); err != nil {
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
	if _, err := db.sql.Exec(`CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("rollup: create schema_meta: %w", err)
	}
	version, err := db.schemaVersion()
	if err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		if _, err := db.sql.Exec(migrations[i]); err != nil {
			return fmt.Errorf("rollup: apply migration %d: %w", i+1, err)
		}
	}
	if version < len(migrations) {
		if _, err := db.sql.Exec(`DELETE FROM schema_meta`); err != nil {
			return fmt.Errorf("rollup: reset schema_meta: %w", err)
		}
		if _, err := db.sql.Exec(`INSERT INTO schema_meta (version) VALUES (?)`, len(migrations)); err != nil {
			return fmt.Errorf("rollup: record schema version: %w", err)
		}
	}
	return nil
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
