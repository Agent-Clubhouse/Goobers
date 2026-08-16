// Package intake is the source watermark store (#1922, design §3.1, §4.3).
//
// It records two facts about a run, and nothing else:
//
//   - that its journal has advanced to some sequence, so the projector knows
//     there is work;
//   - that retention intends to remove it, BEFORE the journal is unlinked.
//
// # Why this is a separate database from read.db
//
// Three reasons, each independently sufficient:
//
//  1. Out-of-process writers. `goobers run`, the stalled-run terminalizer, and
//     retention all record intake, and they are not the projector. Putting
//     intake in read.db would contradict the sole-writer handle that makes the
//     projector's commit order meaningful (§6.1).
//  2. The rebuild barrier. An external process can open the old epoch just
//     before a rebuild's barrier and keep writing to the old inode — those
//     watermarks would be lost with the file. Intake is never rebuilt, so a
//     writer that never noticed the swap still records correctly.
//  3. Windows. The old file cannot be removed while an external process holds
//     it, so a rebuild that had to replace intake would fail outright.
//
// # Why the acknowledgement cannot be transactional
//
// SQLite's WAL provides no atomic commit across attached databases, so the
// projection commit (in read.db) and the intake acknowledgement (here) cannot be
// one transaction. The protocol is what makes that safe rather than the
// transaction: commit the projection first, then delete the marker under a
// guard. See Ack.
//
// # Failure is degradation, not an error
//
// A write here that fails after bounded retries is logged and counted, never
// fatal to the run. That run's discovery falls back to the repair sweep, which
// is slower but complete. The alternative — failing a run because a read-model
// optimisation could not record a hint — would make the read model a
// availability dependency of execution, which is exactly backwards.
package intake

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// FileName is the intake store's name inside the instance directory.
const FileName = "intake.db"

// dsnParams configures every connection.
//
// _txlock=immediate makes each explicit write transaction acquire its lock at
// BEGIN, avoiding the non-waitable read-to-write upgrade race between processes.
const dsnParams = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"

const busyRetryMaxAttempts = 12

// timeFormat matches read.db's, so the two stores' timestamps are comparable
// without conversion when a human is reading both.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Store is the intake database.
type Store struct {
	db   *sql.DB
	path string
}

// Marker is one pending piece of intake.
type Marker struct {
	RunID string
	// SourceSeq is the highest journal sequence a writer has reported.
	SourceSeq uint64
	// Removing reports that retention intends to delete this run.
	Removing   bool
	ObservedAt time.Time
}

// Open opens or creates the intake store.
//
// Any process may call this. There is deliberately no ownership check and no
// lock file: intake is written by whichever process happens to advance a run,
// and requiring an owner would mean a run started by `goobers run` while the
// daemon is down records nothing.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", fileURI(path)+dsnParams)
	if err != nil {
		return nil, fmt.Errorf("intake: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("intake: initialise schema in %s: %w", path, err)
	}
	return store, nil
}

func (s *Store) migrate(ctx context.Context) error {
	var err error
	for attempt := 1; attempt <= busyRetryMaxAttempts; attempt++ {
		err = s.migrateOnce(ctx)
		if err == nil || !isSQLiteBusy(err) {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt) * 20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func (s *Store) migrateOnce(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_meta: %w", err)
	}
	version, err := schemaVersionTx(ctx, tx)
	if err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("store schema version %d is newer than this build supports (%d)",
			version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_meta`); err != nil {
			return fmt.Errorf("reset schema_meta after migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_meta (version) VALUES (?)`, i+1); err != nil {
			return fmt.Errorf("record schema version %d: %w", i+1, err)
		}
	}
	return tx.Commit()
}

func schemaVersionTx(ctx context.Context, tx *sql.Tx) (int, error) {
	var version int
	err := tx.QueryRowContext(ctx, `SELECT version FROM schema_meta LIMIT 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

// Path reports the file backing this store.
func (s *Store) Path() string { return s.path }

// Close releases the handle.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Observed records that a run's journal has advanced to seq.
//
// # A newer observation cancels stale removal intent
//
// The upsert clears `removing`. That is not incidental — it is the one place a
// writer overrides retention, and it is the correct direction.
//
// If retention crashes between recording intent and unlinking the journal, and
// the run then advances or is resumed, a higher source_seq is PROOF the intent
// is stale: a removed journal cannot produce new events. Without clearing the
// flag, the projector would take the removal branch and delete a live run's
// rows — losing a run that is actively executing because a retention pass died
// half-finished.
//
// The guard is `excluded.source_seq >= run_intake.source_seq`, so an
// out-of-order write from a slow process cannot rewind the watermark. Note the
// `>=` rather than `>`: an equal sequence still clears stale removal intent,
// which matters when retention marked a run that had already stopped advancing.
func (s *Store) Observed(ctx context.Context, runID string, journalSeq uint64) error {
	_, err := s.execWriteWithBusyRetry(ctx, `
		INSERT INTO run_intake (run_id, source_seq, removing, observed_at)
		VALUES (?, ?, 0, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			source_seq  = excluded.source_seq,
			removing    = 0,
			observed_at = excluded.observed_at
		WHERE excluded.source_seq >= run_intake.source_seq`,
		runID, journalSeq, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("intake: observe %s at %d: %w", runID, journalSeq, err)
	}
	return nil
}

// Removing records retention's intent to delete a run, before the unlink.
//
// The ordering is intent → unlink → project → confirm. Recording intent first is
// what makes an interrupted retention pass recoverable: a marker with no journal
// behind it tells the projector to emit `run.removed` and drop the row, whereas
// an unlink with no marker leaves a projected row whose journal has vanished,
// discoverable only by the repair sweep.
//
// It does NOT clear the source sequence. The sequence is what a later Observed
// compares against to decide the intent is stale.
func (s *Store) Removing(ctx context.Context, runID string) error {
	_, err := s.execWriteWithBusyRetry(ctx, `
		INSERT INTO run_intake (run_id, source_seq, removing, observed_at)
		VALUES (?, 0, 1, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			removing    = 1,
			observed_at = excluded.observed_at`,
		runID, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("intake: mark %s for removal: %w", runID, err)
	}
	return nil
}

// Pending returns markers for the projector to drain, oldest first.
func (s *Store) Pending(ctx context.Context, limit int) ([]Marker, error) {
	if limit <= 0 {
		limit = defaultPendingLimit
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, source_seq, removing, observed_at
		FROM run_intake
		ORDER BY observed_at ASC, run_id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("intake: read pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Marker
	for rows.Next() {
		var (
			marker     Marker
			removing   int
			observedAt string
		)
		if err := rows.Scan(&marker.RunID, &marker.SourceSeq, &removing, &observedAt); err != nil {
			return nil, fmt.Errorf("intake: scan pending: %w", err)
		}
		marker.Removing = removing != 0
		if marker.ObservedAt, err = time.Parse(timeFormat, observedAt); err != nil {
			return nil, fmt.Errorf("intake: parse observed_at %q: %w", observedAt, err)
		}
		out = append(out, marker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("intake: pending rows: %w", err)
	}
	return out, nil
}

// defaultPendingLimit bounds one drain pass. Restart is O(active + pending), and
// pending is typically tens of rows — the limit exists so a pathological backlog
// is drained across several passes rather than in one unbounded read.
const defaultPendingLimit = 512

// Ack removes a marker AFTER its projection has committed.
//
// # The guard is the protocol
//
// This runs after the read.db transaction commits, and cannot be part of it —
// SQLite's WAL gives no atomic commit across databases. Two conditions make that
// safe:
//
//	source_seq <= ?   a newer append left a HIGHER sequence, so the delete
//	                  no-ops, the marker survives, and the projector revisits.
//	                  Without it, work that arrived during projection would be
//	                  acknowledged as done and silently lost until the sweep.
//
//	removing = 0      a removal marker can never be consumed as progress. If it
//	                  could, a run projected normally while retention had already
//	                  recorded intent would clear the intent, and the unlink
//	                  would leave a projected row with no journal.
//
// A crash between the commit and this call simply reprocesses: the projection is
// idempotent on source position, so the second pass changes nothing.
//
// It reports whether the marker was actually removed, so a caller can tell
// "acknowledged" from "superseded while I was working" without a second query.
func (s *Store) Ack(ctx context.Context, runID string, projectedSeq uint64) (bool, error) {
	result, err := s.execWriteWithBusyRetry(ctx, `
		DELETE FROM run_intake
		WHERE run_id = ? AND source_seq <= ? AND removing = 0`,
		runID, projectedSeq)
	if err != nil {
		return false, fmt.Errorf("intake: ack %s at %d: %w", runID, projectedSeq, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("intake: ack %s rows affected: %w", runID, err)
	}
	return affected > 0, nil
}

// AckRemoval clears a removal marker after the projection has recorded the
// removal.
//
// Separate from Ack because it is the confirm step of intent → unlink → project
// → confirm, and because it deliberately carries no source_seq guard: once the
// journal is unlinked there is no sequence left to compare against. The
// protection against clearing a LIVE run's marker is upstream — Observed clears
// `removing` when a newer sequence proves the intent stale, so a marker that is
// still `removing = 1` at this point has not been contradicted.
func (s *Store) AckRemoval(ctx context.Context, runID string) error {
	if _, err := s.execWriteWithBusyRetry(ctx,
		`DELETE FROM run_intake WHERE run_id = ? AND removing = 1`, runID); err != nil {
		return fmt.Errorf("intake: ack removal of %s: %w", runID, err)
	}
	return nil
}

// execWriteWithBusyRetry runs a mutation in an immediate transaction and
// retries the whole transaction when SQLite reports resolvable contention.
func (s *Store) execWriteWithBusyRetry(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var err error
	for attempt := 1; attempt <= busyRetryMaxAttempts; attempt++ {
		var result sql.Result
		result, err = s.execWriteOnce(ctx, query, args...)
		if err == nil || !isSQLiteBusy(err) {
			return result, err
		}
		timer := time.NewTimer(time.Duration(attempt) * 20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, err
}

func (s *Store) execWriteOnce(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xFF
	return code == 5 || code == 6
}

// Get returns one run's marker, if any.
func (s *Store) Get(ctx context.Context, runID string) (Marker, bool, error) {
	var (
		marker     Marker
		removing   int
		observedAt string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT run_id, source_seq, removing, observed_at
		FROM run_intake WHERE run_id = ?`, runID).
		Scan(&marker.RunID, &marker.SourceSeq, &removing, &observedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Marker{}, false, nil
	case err != nil:
		return Marker{}, false, fmt.Errorf("intake: get %s: %w", runID, err)
	}
	marker.Removing = removing != 0
	if marker.ObservedAt, err = time.Parse(timeFormat, observedAt); err != nil {
		return Marker{}, false, fmt.Errorf("intake: parse observed_at %q: %w", observedAt, err)
	}
	return marker, true, nil
}

// Count reports how many markers are pending, for the freshness surface.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_intake`).Scan(&n); err != nil {
		return 0, fmt.Errorf("intake: count: %w", err)
	}
	return n, nil
}

// PathFor returns the intake store's path inside an instance directory.
func PathFor(instanceDir string) string { return filepath.Join(instanceDir, FileName) }

// fileURI renders a path as a file: URI so DSN parameters apply.
//
// The absolute path must be given a leading slash before it reaches url.URL.
// On Windows filepath.ToSlash yields "C:/dir/intake.db", whose first segment
// contains a colon; url.URL.String then prefixes "./" so the segment cannot be
// mistaken for a scheme, producing "file:./C:/dir/intake.db" — a relative URI
// SQLite cannot open (SQLITE_CANTOPEN). Rooting the path first yields
// "file:///C:/dir/intake.db". POSIX paths are already rooted and unaffected.
func fileURI(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + (&url.URL{Path: slashed}).EscapedPath()
}

// Store satisfies readmodel.Intake. The assertion lives here rather than in
// readmodel, which must not import this package: intake is a SEPARATE database
// with its own lifecycle, and a dependency from the projected-facts store to the
// watermark store would be the coupling §3.1 exists to prevent.
var _ interface {
	Observed(ctx context.Context, runID string, journalSeq uint64) error
	Removing(ctx context.Context, runID string) error
} = (*Store)(nil)
