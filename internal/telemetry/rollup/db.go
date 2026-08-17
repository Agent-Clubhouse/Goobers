package rollup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite" // pure-Go driver: no cgo, matches the local-runner's
	// single-binary/no-service-dependency posture (ARCHITECTURE.md §3.1).
)

// DB is an open handle to the local telemetry rollup (telemetry.db, TEL-032).
// Per-run data is derived and rebuildable from journals. Lifetime first-success
// milestones are retained independently once observed because their source run
// journals may be removed by policy.
type DB struct {
	// sql is the WRITER handle: one connection, _txlock=immediate. Every Exec,
	// transaction, and migration goes through it.
	sql *sql.DB
	// reader is a read-only pool sized to the machine, or nil when a read-only
	// handle could not be opened (see openReaderPool). Queries route here.
	//
	// Before this, one connection served every reader AND the writer, so
	// concurrent reads gained nothing from each other. Measured on a 40,000-row
	// table with eight concurrent aggregate queries: serial 48.37ms, concurrent
	// 48.62ms — a 0.99x "speedup". That is design §5.2's "today every reader
	// serializes behind every reader *and* the writer, so the Overview's five
	// concurrent requests have their queries serialized and an analytics
	// aggregate blocks every list".
	reader       *sql.DB
	readerMu     sync.RWMutex
	readerClosed bool
	schedulerMu  sync.Mutex
	// path is retained so the reader pool can be reopened after Compact.
	path string
}

// readDB returns the handle queries should use: the read-only pool when
// available, otherwise the writer.
//
// The fallback is not a degraded mode to be alarmed about — an in-memory
// database, or a platform where the read-only URI cannot be formed, simply keeps
// today's behaviour, which is correct if slower.
func (db *DB) readDB() *sql.DB {
	db.readerMu.RLock()
	reader := db.reader
	db.readerMu.RUnlock()
	if reader != nil {
		return reader
	}
	// Reopen lazily after Compact released the pool, so compaction costs one
	// reopen rather than permanently demoting the process to a single handle.
	db.readerMu.Lock()
	defer db.readerMu.Unlock()
	if db.reader == nil && !db.readerClosed {
		db.reader = openReaderPool(db.path)
	}
	if db.reader != nil {
		return db.reader
	}
	return db.sql
}

// closeReaderPool releases the read-only pool. Used by Compact, which needs
// exclusive access, and by Close.
func (db *DB) closeReaderPool() {
	db.readerMu.Lock()
	defer db.readerMu.Unlock()
	if db.reader != nil {
		_ = db.reader.Close()
		db.reader = nil
	}
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
//   - synchronous(NORMAL): in WAL mode this fsyncs at checkpoints rather than on
//     every commit. The rollup is DERIVED, rebuildable data (rollup.Rebuild
//     wipes and re-ingests from journals, which remain the authoritative record
//     and keep their own per-append fsync), so paying a full fsync per commit
//     buys durability for something that can be reconstructed — while the
//     scheduler's write path competes with every read in the same process
//     (design §5.2). A crash can lose recent projected rows; the next ingest or
//     rebuild restores them.
const dsnParams = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"

// Open opens (creating if needed) the SQLite rollup at path and applies any
// pending forward migrations (seeds the V1 upgrade story, #33). Connection
// behaviour under concurrent access (WAL mode, busy_timeout, immediate write
// locks) is configured via dsnParams so it applies to every pooled connection.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		return nil, fmt.Errorf("rollup: open %s: %w", path, err)
	}
	// One connection on the WRITER, which is what avoids "database is locked"
	// between our own writers and what keeps the #1128 first-open race
	// single-threaded through the WAL switch and migrations.
	sqlDB.SetMaxOpenConns(1)
	db := &DB{sql: sqlDB, path: path}
	if err := db.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	// The reader pool opens only AFTER migrations complete. A read-only handle
	// cannot create the database, switch it to WAL, or run a migration, so
	// opening it earlier would either fail or race the schema it depends on.
	db.reader = openReaderPool(path)
	return db, nil
}

// readerDSNParams configures the read-only pool.
//
// Deliberately different from dsnParams in two ways. There is no
// _txlock=immediate: that exists so our explicit WRITE transactions take the
// write lock at BEGIN, and forcing it on a reader would make every read
// transaction contend for a lock it never needs. And mode=ro is what makes the
// handle structurally unable to write — the same "reads never write" boundary
// §3.1 enforces with types at the service layer, enforced here by the driver.
const readerDSNParams = "?_pragma=busy_timeout(5000)&mode=ro"

// openReaderPool opens a read-only connection pool over an existing database.
//
// Returns nil rather than an error when a read-only handle cannot be formed. The
// pool is a performance property, not a correctness one: falling back to the
// single writer handle preserves exactly today's behaviour, and failing Open
// because a pool could not be created would turn an optimization into an outage.
func openReaderPool(path string) *sql.DB {
	// mode=ro requires a file: URI, whereas the writer deliberately keeps the
	// literal path left of the "?" to avoid URI-encoding pitfalls with Windows
	// backslashes and drive letters. So the URI is built explicitly here rather
	// than by string concatenation.
	uri := fileURI(path)
	if uri == "" {
		return nil
	}
	readDB, err := sql.Open("sqlite", uri+readerDSNParams)
	if err != nil {
		return nil
	}
	// Sized to the machine: readers no longer queue behind one another. The
	// writer keeps its own single connection, so a long analytics aggregate can
	// no longer block a list page.
	readDB.SetMaxOpenConns(readerPoolSize())
	readDB.SetMaxIdleConns(readerPoolSize())
	// Prove the handle works before adopting it; a pool that fails on first use
	// would surface as a query error rather than a clean fallback.
	if err := readDB.Ping(); err != nil {
		_ = readDB.Close()
		return nil
	}
	return readDB
}

// readerPoolSize bounds the read pool. NumCPU per §5.2, with a floor so a
// single-core machine still gets a usable pool and a ceiling so a very large
// host does not open dozens of file handles for a local store.
func readerPoolSize() int {
	n := runtime.NumCPU()
	if n < 2 {
		return 2
	}
	if n > 16 {
		return 16
	}
	return n
}

// fileURI converts an OS path to a SQLite file: URI.
//
// SQLite's URI parser treats backslashes as ordinary characters, so a Windows
// path must use forward slashes, and a drive-letter path needs the leading
// slash that makes it absolute.
func fileURI(path string) string {
	if path == "" || strings.Contains(path, "?") {
		// A path already carrying query syntax cannot be safely appended to.
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + (&url.URL{Path: slashed}).EscapedPath()
}

// Close closes both handles. The reader is closed first so no query is in
// flight against a database whose writer is going away.
func (db *DB) Close() error {
	db.readerMu.Lock()
	db.readerClosed = true // no lazy reopen after Close
	db.readerMu.Unlock()
	db.closeReaderPool()
	return db.sql.Close()
}

// Compact reclaims disk after large deletions — a retention prune, or scheduler
// journal compaction (#1412) — that free pages but never shrink the file on
// their own. It truncates the WAL back into the main database, VACUUMs (which
// rewrites the file at its true size), then truncates the WAL once more.
// VACUUM needs exclusive access and rewrites the whole database, so the caller
// must run this with the daemon stopped; unlike checkpointWAL's best-effort
// maintenance, a failure here is surfaced so the operator knows compaction did
// not complete.
func (db *DB) Compact(ctx context.Context) error {
	// VACUUM rewrites the whole database and needs exclusive access, so the
	// read-only pool's idle connections must be released first — otherwise a
	// pool that is merely IDLE (not querying) still holds file handles and the
	// vacuum fails with "database is locked". readDB reopens it on the next read.
	//
	// This is the one operation where the reader pool's existence is not
	// transparent, which is why it is handled here rather than left to surface
	// as an intermittent compaction failure.
	db.closeReaderPool()
	if _, err := db.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("rollup: checkpoint before vacuum: %w", err)
	}
	if _, err := db.sql.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("rollup: vacuum: %w", err)
	}
	if _, err := db.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("rollup: checkpoint after vacuum: %w", err)
	}
	return nil
}

// PruneSchedulerBefore deletes scheduler_events and scheduler_errors rows whose
// occurred_at predates cutoff — the rollup-side counterpart to compacting the
// instance journal (#1412), so a bloated scheduler_errors table (its rows are
// never overwritten, only appended) can actually shrink. occurred_at is stored
// in a fixed-width, lexicographically-ordered UTC layout, so the string
// comparison is a correct time comparison; rows with a NULL occurred_at are
// conservatively kept. Returns the number of scheduler_events rows removed.
// Call Compact afterward to reclaim the freed pages.
func (db *DB) PruneSchedulerBefore(ctx context.Context, cutoff time.Time) (int, error) {
	ts := formatTime(cutoff)
	if !ts.Valid {
		return 0, nil // a zero cutoff prunes nothing
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rollup: begin scheduler prune tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM scheduler_errors WHERE occurred_at < ?`, ts.String); err != nil {
		return 0, fmt.Errorf("rollup: prune scheduler_errors: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM scheduler_events WHERE occurred_at < ?`, ts.String)
	if err != nil {
		return 0, fmt.Errorf("rollup: prune scheduler_events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rollup: commit scheduler prune: %w", err)
	}
	removed, _ := res.RowsAffected()
	return int(removed), nil
}

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
		if err = db.migrateOnce(context.Background()); err == nil || !isSQLiteBusy(err) {
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
func (db *DB) migrateOnce(ctx context.Context) error {
	tx, err := db.sql.BeginTx(ctx, nil) // BEGIN IMMEDIATE via _txlock=immediate (see dsnParams)
	if err != nil {
		return fmt.Errorf("rollup: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	var schemaMetaExists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_master
			WHERE type = 'table' AND name = 'schema_meta'
		)`).Scan(&schemaMetaExists); err != nil {
		return fmt.Errorf("rollup: inspect schema_meta: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("rollup: create schema_meta: %w", err)
	}
	version, err := schemaVersionTx(ctx, tx, schemaMetaExists)
	if err != nil {
		return err
	}
	if version < 0 {
		return fmt.Errorf(
			"rollup: schema version %d is invalid; supported versions are 0 through %d; restore telemetry.db from backup",
			version, len(migrations),
		)
	}
	if version > len(migrations) {
		return fmt.Errorf(
			"rollup: schema version %d is newer than supported version %d; upgrade Goobers to a binary that supports telemetry schema version %d",
			version, len(migrations), version,
		)
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("rollup: apply migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_meta`); err != nil {
			return fmt.Errorf("rollup: reset schema_meta after migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_meta (version) VALUES (?)`, i+1); err != nil {
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
func schemaVersionTx(ctx context.Context, tx *sql.Tx, schemaMetaExisted bool) (int, error) {
	var count, version int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MIN(version), 0)
		FROM schema_meta`).Scan(&count, &version); err != nil {
		return 0, fmt.Errorf("rollup: read schema version: %w", err)
	}
	if count == 0 && !schemaMetaExisted {
		return 0, nil
	}
	if count != 1 {
		return 0, fmt.Errorf(
			"rollup: schema_meta must contain exactly one version row, found %d; restore telemetry.db from backup",
			count,
		)
	}
	return version, nil
}
