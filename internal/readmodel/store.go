// Package readmodel is the portal's run read model: a small, separate SQLite
// store (read.db) holding complete run rows so a list page can be answered with
// zero journal opens.
//
// It implements design docs/design/portal-read-architecture.md §5. Two things
// distinguish it from internal/telemetry/rollup, which it does not replace:
//
//   - Size and lifecycle. read.db is gated on 191 MB of run events; telemetry.db
//     is 547 MB gated on 2,263 MB of spans (§5.1). Splitting them means cold
//     start pays for the data the product IS rather than the data that decorates
//     it, the two get the independent retention policies they already need, and
//     read.db stays small enough that rebuild is a whole-store operation.
//   - Completeness. Every column exists so a list is one indexed query with no
//     per-row journal hydration. Measured today: a 50-row page opens 51 journals.
//
// # What this package does NOT yet do
//
// Nothing reads it. §6.6 sequences the transition deliberately: ship read.db
// construction alongside the existing store first, build it from journals, then
// cut reads over behind a flag with the old path intact, and only later drop
// what it supersedes. Rollback at this stage is deleting a file.
//
// The projector (§6), the change feed (§4.2), and the query surface (§5.7) land
// in later slices. This is the store and its schema.
package readmodel

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, matching the rest of the tree
)

// FileName is read.db's name within an instance root.
const FileName = "read.db"

// dsnParams configures the writer connection.
//
// Deliberately the same shape as the rollup's, and for the same reasons (#1128):
// DSN-level pragmas apply to every physical connection the pool opens, before
// that connection runs its first statement, whereas a post-open PRAGMA Exec
// configures only whichever connection happened to run it and leaves the busy
// handler unarmed for the WAL switch itself.
//
// synchronous(NORMAL) because this store is entirely derived and rebuildable
// from journals (§3.2), which keep their own per-append fsync. Paying a full
// fsync per commit would buy durability for something reconstructible while
// competing with every read in the same process.
const dsnParams = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"

// readerDSNParams configures the read-only pool. No _txlock=immediate: that
// exists so explicit WRITE transactions take the lock at BEGIN, and forcing it
// on a reader would make every read contend for a lock it never needs.
//
// mode=ro is the driver-level half of §3.1's "reads never write" boundary.
const readerDSNParams = "?_pragma=busy_timeout(5000)&mode=ro"

// Store is an open handle to read.db.
type Store struct {
	// measurement supplies the telemetry-derived flags the journal cannot
	// provide (#1782). Nil is a supported configuration: a topology with no
	// telemetry projects everything except the population flags.
	measurement MeasurementSource

	// writer is the sole writable handle. §5.2/§6.1: the projector owns the only
	// write path to projected facts, and one connection is what keeps the
	// first-open WAL switch and migrations single-threaded.
	writer *sql.DB
	// reader is a read-only pool. Reads must not serialize behind each other or
	// behind the writer — measured on the rollup, eight concurrent aggregates
	// gained nothing from concurrency (0.99x) until it was split.
	reader *sql.DB
	path   string

	// handles guards writer and reader, which Close mutates and every query
	// reads. A read lock on the query path is uncontended in the common case and
	// is the difference between a clean shutdown and a data race.
	handles sync.RWMutex

	// clock stamps the one change row that cannot be derived from a journal:
	// run.removed, whose whole point is that the journal is gone. Injectable so
	// a test can assert removal ordering without sleeping.
	clock func() time.Time
}

// now returns the store's clock, defaulting to the wall clock.
func (s *Store) now() time.Time {
	if s.clock == nil {
		return time.Now().UTC()
	}
	return s.clock().UTC()
}

// SetClock overrides the clock. Test-only; production leaves it nil.
func (s *Store) SetClock(clock func() time.Time) { s.clock = clock }

// State is projection_state's single row.
type State struct {
	SchemaVersion   int
	Epoch           string
	MinChangeSeq    uint64
	ProjectionFloor *time.Time
	LastSweepAt     *time.Time
	BuiltAt         time.Time
}

// Open opens (creating if needed) read.db at path and applies pending
// migrations.
//
// A store with no projection_state row is initialised with a FRESH epoch. That
// is the whole point of the epoch being opaque (§4.2): it must be independent of
// any value read from the old or the new store, because a counter recovered from
// a store that a rebuild replaces would restart and recreate the exact cursor
// collision the epoch exists to prevent.
func Open(path string) (*Store, error) {
	writer, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		return nil, fmt.Errorf("readmodel: open %s: %w", path, err)
	}
	writer.SetMaxOpenConns(1)

	store := &Store{writer: writer, path: path}
	if err := store.migrate(context.Background()); err != nil {
		_ = writer.Close()
		return nil, err
	}
	// The reader pool opens only after migrations: a read-only handle cannot
	// create the database, switch it to WAL, or run a migration.
	store.reader = openReaderPool(path)
	return store, nil
}

// Close releases both handles, reader first so no query is in flight against a
// database whose writer is going away.
//
// Guarded because Close races reads. The handle fields are read by every query
// through readDB/writeDB, and reassigned by reopen after an epoch swap; until
// this lock existed that was an unsynchronised data race — caught by the race
// detector on main, but reachable in production any time a read overlaps daemon
// shutdown, which is exactly when a long-lived SSE subscription is still
// draining.
//
// # The handles are closed but NOT nilled
//
// An earlier version of this fix set them to nil so a post-Close query would
// "fail cleanly". It does not: QueryContext on a nil *sql.DB dereferences it and
// SEGFAULTS (database/sql.(*DB).conn, sql.go:1317). That turned a data race into
// a panic, which the race shard duly caught.
//
// A CLOSED *sql.DB is the correct thing to keep: it is safe to call, returns
// "sql: database is closed", and Close itself is idempotent. So a read that
// overlaps shutdown gets an error it can report rather than taking the process
// down with it.
func (s *Store) Close() error {
	s.handles.Lock()
	defer s.handles.Unlock()
	var readerErr error
	if s.reader != nil {
		readerErr = s.reader.Close()
	}
	if s.writer == nil {
		return readerErr
	}
	return errors.Join(readerErr, s.writer.Close())
}

// writeDB returns the writable handle, under the same guard as readDB.
//
// Every write goes through here rather than touching s.writer, so Close cannot
// race a commit in flight. After Close this is a closed handle, not nil — see
// Close for why that distinction is load-bearing.
func (s *Store) writeDB() *sql.DB {
	s.handles.RLock()
	defer s.handles.RUnlock()
	return s.writer
}

// readDB returns the handle queries should use.
func (s *Store) readDB() *sql.DB {
	s.handles.RLock()
	defer s.handles.RUnlock()
	if s.reader != nil {
		return s.reader
	}
	return s.writer
}

// State returns the store's projection state.
func (s *Store) State(ctx context.Context) (State, error) {
	row := s.readDB().QueryRowContext(ctx, `
		SELECT schema_version, epoch, min_change_seq, projection_floor, last_sweep_at, built_at
		FROM projection_state WHERE id = 1`)
	var (
		st                  State
		floor, sweep, built sql.NullString
	)
	if err := row.Scan(&st.SchemaVersion, &st.Epoch, &st.MinChangeSeq, &floor, &sweep, &built); err != nil {
		return State{}, fmt.Errorf("readmodel: read projection state: %w", err)
	}
	var err error
	if st.ProjectionFloor, err = optionalTime(floor); err != nil {
		return State{}, err
	}
	if st.LastSweepAt, err = optionalTime(sweep); err != nil {
		return State{}, err
	}
	if built.Valid {
		if st.BuiltAt, err = time.Parse(timeFormat, built.String); err != nil {
			return State{}, fmt.Errorf("readmodel: parse built_at %q: %w", built.String, err)
		}
	}
	return st, nil
}

// migrate applies pending forward migrations and seeds projection_state.
func (s *Store) migrate(ctx context.Context) error {
	version, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	if version > len(migrations) {
		// A store written by a newer binary. Refusing is right: silently running
		// against a schema this build does not understand is how a read model
		// starts answering subtly wrong questions.
		return fmt.Errorf("readmodel: store schema version %d is newer than this build supports (%d)",
			version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.writer.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("readmodel: begin migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("readmodel: apply migration %d: %w", i+1, err)
		}
		if err := seedState(ctx, tx, i+1); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("readmodel: commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

// schemaVersion reports how many migrations have run, or 0 for a fresh store.
func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.writer.QueryRowContext(ctx,
		`SELECT schema_version FROM projection_state WHERE id = 1`).Scan(&version)
	switch {
	case err == nil:
		return version, nil
	case isMissingTable(err):
		return 0, nil
	default:
		return 0, fmt.Errorf("readmodel: read schema version: %w", err)
	}
}

// seedState creates or advances projection_state within a migration.
//
// The epoch is minted ONCE, on first creation, and preserved across later
// migrations. A migration is not a rebuild: it does not replace the file, so
// existing cursors stay valid and re-minting would force every connected client
// to resnapshot for no reason (§4.2's epoch_changed is for a new FILE).
func seedState(ctx context.Context, tx *sql.Tx, version int) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE projection_state SET schema_version = ? WHERE id = 1`, version)
	if err != nil {
		return fmt.Errorf("readmodel: advance schema version: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("readmodel: advance schema version: %w", err)
	}
	if affected > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projection_state (id, schema_version, epoch, min_change_seq, built_at)
		VALUES (1, ?, ?, 0, ?)`,
		version, NewEpoch(), time.Now().UTC().Format(timeFormat),
	); err != nil {
		return fmt.Errorf("readmodel: seed projection state: %w", err)
	}
	return nil
}

// NewEpoch mints a fresh opaque epoch identity.
//
// Minted rather than derived, because §4.2 requires the epoch to be independent
// of any value read from either store: a counter living in projection_state
// would be lost when a rebuild replaces read.db, restart from zero, and recreate
// the exact cursor collision the epoch exists to prevent.
//
// 128 random bits rather than the ULID the design names. The properties §4.2
// actually requires are opacity, uniqueness per build, and EQUALITY semantics —
// it is explicit that "epochs need equality semantics, not ordering". A ULID's
// distinguishing feature is that it sorts by time, which is not wanted here and
// would invite exactly the ordering comparison the section warns against. This
// also avoids adding a module dependency for a 16-byte identifier, and
// crypto/rand is already the house pattern for ids in this tree.
//
// Falls back to a timestamp-derived value if the system entropy source fails,
// which cannot happen in practice but must not panic a daemon start; uniqueness
// then rests on the clock, which is sufficient because a rebuild is a rare,
// human- or scheduler-triggered event rather than a hot path.
func NewEpoch() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("epoch-%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// timeFormat is fixed-width and UTC so text comparison is a correct time
// comparison — which is what lets started_at lead an ordering index.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// optionalTime parses a nullable timestamp column.
func optionalTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := time.Parse(timeFormat, ns.String)
	if err != nil {
		return nil, fmt.Errorf("readmodel: parse timestamp %q: %w", ns.String, err)
	}
	return &t, nil
}

// isMissingTable reports whether err is SQLite's "no such table".
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// openReaderPool opens a read-only pool, or returns nil if one cannot be formed.
//
// nil rather than an error: the pool is a performance property, not a
// correctness one, and failing Open because a pool could not be created would
// turn an optimization into an outage. The fallback is the writer handle.
func openReaderPool(path string) *sql.DB {
	uri := fileURI(path)
	if uri == "" {
		return nil
	}
	readDB, err := sql.Open("sqlite", uri+readerDSNParams)
	if err != nil {
		return nil
	}
	size := readerPoolSize()
	readDB.SetMaxOpenConns(size)
	readDB.SetMaxIdleConns(size)
	if err := readDB.Ping(); err != nil {
		_ = readDB.Close()
		return nil
	}
	return readDB
}

// readerPoolSize bounds the read pool (§5.2: NumCPU), with a floor so a
// single-core machine still gets concurrency and a ceiling so a large host does
// not open dozens of handles for a local store.
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

// fileURI converts an OS path to a SQLite file: URI, which mode=ro requires.
//
// SQLite's URI parser treats backslashes as ordinary characters, so a Windows
// path needs forward slashes, and a drive-letter path needs the leading slash
// that makes it absolute. Escaping matters too: an unescaped space truncates the
// DSN at the space.
func fileURI(path string) string {
	if path == "" || strings.Contains(path, "?") {
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
