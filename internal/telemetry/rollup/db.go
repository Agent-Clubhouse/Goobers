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
// pending forward migrations (seeds the V1 upgrade story, #33).
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("rollup: open %s: %w", path, err)
	}
	// A single connection avoids SQLite "database is locked" errors from our
	// own concurrent writers; the rollup is a local, single-process store.
	sqlDB.SetMaxOpenConns(1)
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
