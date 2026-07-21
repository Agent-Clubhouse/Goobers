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

// dsnParams configures the rollup connection at the DSN level so the modernc
// driver applies every setting to EACH physical connection the pool opens,
// before that connection runs its first statement (#1128). This is the crucial
// difference from setting them with a post-open `PRAGMA` Exec: an Exec only
// configures whichever single connection happened to run it, and — worse —
// leaves the busy handler unarmed for the WAL-mode switch and hot-journal
// recovery that a concurrent first-open must survive, so several processes
// racing to open the SAME brand-new telemetry.db (e.g. `goobers up` starting at
// the same instant as a `goobers telemetry` query) stampede the WAL init and
// surface SQLITE_BUSY immediately with no wait. The three params:
//   - busy_timeout(5000): wait out ordinary lock contention for up to 5s
//     (retrying internally) instead of failing immediately — armed from the
//     connection's very first statement, including the WAL switch itself.
//   - journal_mode(WAL): let a reader (a `goobers telemetry`/`goobers trace`
//     query) proceed concurrently with a writer (the daemon's incremental
//     ingest on run finish, #127) instead of blocking behind SQLITE_BUSY.
//   - _txlock=immediate: our only explicit transactions are writers (migration
//     here, ingest in ingest.go), so taking the write lock at BEGIN turns a
//     fragile read→write lock upgrade — which SQLite can fail with a
//     non-waitable SQLITE_BUSY to avoid deadlock, defeating busy_timeout — into
//     an ordinary lock wait the busy handler serializes cleanly. Autocommit
//     reads (the query paths) don't use explicit transactions, so WAL reader
//     concurrency is unaffected.
//
// The literal path is kept left of the "?" (not a file: URI) so it is used
// verbatim as an OS path, avoiding URI-encoding pitfalls with Windows
// backslashes and drive letters.
const dsnParams = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"

// Open opens (creating if needed) the SQLite rollup at path and applies any
// pending forward migrations (seeds the V1 upgrade story, #33). Connection
// behaviour under concurrent access (WAL mode, busy_timeout, immediate write
// locks) is configured via dsnParams so it applies to every pooled connection.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path+dsnParams)
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

// checkpointWAL runs a TRUNCATE-mode WAL checkpoint (#530): writes every WAL
// frame back into the main db file and, only if that fully succeeds,
// truncates the -wal file to zero bytes — bounding its otherwise-unbounded
// growth across repeated incremental ingests. Best-effort: retried against
// ordinary lock contention like every other pragma here, but a caller must
// never fail its own operation over a checkpoint that couldn't complete
// (e.g. a concurrent reader transaction from another process legitimately
// held it back) — checkpointing is a maintenance step, not a correctness
// requirement, since WAL mode already serves correct reads without it.
func checkpointWAL(sqlDB *sql.DB) {
	_ = execWithBusyRetry(sqlDB, `PRAGMA wal_checkpoint(TRUNCATE)`)
}

// migrate runs the entire first-open setup — creating schema_meta, reading the
// current version, and applying every pending migration — inside ONE
// immediate-mode write transaction (migrateOnce). _txlock=immediate (see
// dsnParams) makes Begin() take the write lock at BEGIN, and busy_timeout waits
// on that cleanly.
//
// Doing this same work as separate autocommit statements — a bare
// CREATE TABLE, then per-migration transactions — was the #1128 flake: each
// autocommit statement does an internal read→write lock upgrade, and SQLite
// fails that upgrade with a *non-waitable* SQLITE_BUSY (deadlock avoidance,
// which busy_timeout's handler deliberately does NOT wait out) when N
// connections open the same fresh telemetry.db at once (e.g. `goobers up`
// racing a `goobers telemetry` query at instance startup). Serializing every
// first-opener on one up-front write lock removes the upgrade race entirely.
//
// The retry loop is now only a thin backstop for SQLITE_BUSY_SNAPSHOT — the
// one case busy_timeout's C-level handler still cannot wait out (a stale read
// snapshot can only be abandoned and retaken, not blocked on) — and should
// rarely, if ever, be reached now that the write lock is acquired at BEGIN.
func (db *DB) migrate() error {
	var err error
	for attempt := 1; attempt <= busyRetryMaxAttempts; attempt++ {
		if err = db.migrateOnce(); err == nil || !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
	}
	return err
}

// busyRetryMaxAttempts bounds migrate's SQLITE_BUSY_SNAPSHOT backstop and
// checkpointWAL's best-effort retry — the only schema-shaped/maintenance work
// this package does that multiple processes can race on simultaneously
// (ordinary queries elsewhere rely on busy_timeout alone). Sized with real
// headroom rather than the tightest value that happened to pass a few runs.
const busyRetryMaxAttempts = 12

// execWithBusyRetry runs a single autocommit statement, retrying on
// SBUSY/LOCKED with a short linear backoff. Used by checkpointWAL, whose
// PRAGMA wal_checkpoint can lose the write lock to a concurrent process's
// reader/writer and must not fail the caller over it.
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

// migrateOnce performs the whole schema setup in a single transaction. Pairing
// each migration with its schema_meta version bump keeps them atomic, so a
// crash can never leave the recorded version out of sync with the applied DDL:
// every migration so far is CREATE TABLE/INDEX IF NOT EXISTS (safe to re-run),
// but that won't always hold — a future ALTER TABLE is not idempotent, and
// re-applying it after a crash that recorded no version bump would fail (issue
// #129). Folding the full batch into one transaction extends that same
// all-or-nothing guarantee across every pending migration. SQLite supports
// transactional DDL, so this is safe here specifically — this package only ever
// targets SQLite (modernc.org/sqlite).
func (db *DB) migrateOnce() error {
	tx, err := db.sql.Begin() // BEGIN IMMEDIATE via _txlock=immediate (see dsnParams)
	if err != nil {
		return fmt.Errorf("rollup: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("rollup: create schema_meta: %w", err)
	}
	version, err := schemaVersionTx(tx)
	if err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.Exec(migrations[i]); err != nil {
			return fmt.Errorf("rollup: apply migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`DELETE FROM schema_meta`); err != nil {
			return fmt.Errorf("rollup: reset schema_meta after migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_meta (version) VALUES (?)`, i+1); err != nil {
			return fmt.Errorf("rollup: record schema version %d: %w", i+1, err)
		}
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

// schemaVersionTx reads the recorded schema version within the migration
// transaction (so the read shares the write lock migrateOnce already holds — no
// separate autocommit read that could race a concurrent first-opener).
func schemaVersionTx(tx *sql.Tx) (int, error) {
	var version int
	err := tx.QueryRow(`SELECT version FROM schema_meta LIMIT 1`).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("rollup: read schema version: %w", err)
	}
	return version, nil
}
