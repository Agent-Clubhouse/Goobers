package readmodel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Epoch rebuild (#1925, design §6.5).
//
// # Why a rename is not enough
//
// "Build a temp file and rename it over the old one" is unsafe for a WAL
// database with pooled readers. The `-wal` and `-shm` files cannot be atomically
// swapped with the main file by one rename; on Unix, connections already in the
// pool keep reading the OLD inode after the rename and never see the new data;
// on Windows, replacing an open database generally fails outright.
//
// So the swap is explicit: quiesce, close the entire reader pool, move the file,
// reopen. Requests during that window get 503 + Retry-After — a brief refusal,
// never a stale inode served as though it were current.

// RebuildState is a rebuild in progress.
type RebuildState struct {
	// Epoch is the new epoch being built.
	Epoch string
	// Path is the file being built, beside the live one.
	Path string
	// RebuildFromSeq is the old epoch's change.seq at the moment the rebuild
	// started. Everything after it must be caught up before the swap.
	RebuildFromSeq uint64
	StartedAt      time.Time

	store *Store
	next  *Store
}

// rebuildFileName renders the in-progress file's name.
//
// Beside the live database rather than in a temp directory: the swap is a rename
// within one directory, which is atomic on every filesystem we support. Across
// directories it may not be, and a rebuild that lands half-moved is worse than
// one that fails.
func rebuildFileName(dir, epoch string) string {
	return filepath.Join(dir, fmt.Sprintf("read-%s.db", epoch))
}

// BeginRebuild starts building a fresh epoch beside the live store.
//
// The live store stays fully readable throughout. Intake lives in its own
// database and is never rebuilt, so external writers keep recording correctly
// across the entire operation.
func (s *Store) BeginRebuild(ctx context.Context) (*RebuildState, error) {
	from, err := s.LatestChangeSeq(ctx)
	if err != nil {
		return nil, err
	}
	epoch := NewEpoch()
	path := rebuildFileName(filepath.Dir(s.path), epoch)

	// A leftover file from an aborted build would otherwise be opened and
	// migrated as though it were ours, and its rows silently merged into the new
	// epoch. Removing it first makes each rebuild start from nothing.
	if err := removeDatabaseFiles(path); err != nil {
		return nil, err
	}

	next, err := Open(path)
	if err != nil {
		return nil, fmt.Errorf("readmodel: open rebuild target: %w", err)
	}
	if err := next.setEpoch(ctx, epoch); err != nil {
		_ = next.Close()
		return nil, err
	}

	// The floor and tombstones are copied BEFORE any journal is projected.
	//
	// They are derived POLICY state in the old store, not journal facts, so a
	// rebuild from journals alone does not reproduce them. Without this a
	// post-retention rebuild would briefly re-admit every expired journal, burst
	// removals into the change feed, and make rebuild size proportional to total
	// history rather than the retained window — defeating §14.12's rebuild-time
	// and store-size targets at exactly the moment they matter most.
	if err := s.copyPolicyState(ctx, next); err != nil {
		_ = next.Close()
		return nil, err
	}

	return &RebuildState{
		Epoch: epoch, Path: path, RebuildFromSeq: from,
		StartedAt: s.now(), store: s, next: next,
	}, nil
}

// Target is the store being built. Callers project into it.
func (r *RebuildState) Target() *Store { return r.next }

// copyPolicyState copies the projection floor and tombstones into a new epoch.
func (s *Store) copyPolicyState(ctx context.Context, next *Store) error {
	floor, hasFloor, err := s.ProjectionFloor(ctx)
	if err != nil {
		return err
	}
	if hasFloor {
		if err := next.SetProjectionFloor(ctx, floor); err != nil {
			return err
		}
	}

	rows, err := s.readDB().QueryContext(ctx,
		`SELECT run_id, started_at, tombstoned_at, reason FROM tombstone`)
	if err != nil {
		return fmt.Errorf("readmodel: read tombstones: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tx, err := next.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("readmodel: begin tombstone copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for rows.Next() {
		var runID, startedAt, tombstonedAt, reason string
		if err := rows.Scan(&runID, &startedAt, &tombstonedAt, &reason); err != nil {
			return fmt.Errorf("readmodel: scan tombstone: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tombstone (run_id, started_at, tombstoned_at, reason) VALUES (?, ?, ?, ?)
			ON CONFLICT(run_id) DO NOTHING`, runID, startedAt, tombstonedAt, reason); err != nil {
			return fmt.Errorf("readmodel: copy tombstone %s: %w", runID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("readmodel: tombstone rows: %w", err)
	}
	return tx.Commit()
}

// CatchUpRunIDs returns every run that changed in the OLD epoch since the
// rebuild started.
//
// # Why pending intake alone is insufficient
//
// The tempting shortcut is "replay whatever intake still has pending". It loses
// data, invisibly:
//
//	E reads run R at source seq 10.
//	R advances to 11 while E is building.
//	The old epoch — still live — projects 11 and ACKNOWLEDGES R's marker.
//	The barrier sees no pending marker, and publishes E stale at 10.
//
// Nothing reports it. The change feed is the second source precisely because it
// records what the old epoch applied, whether or not the marker survived.
func (r *RebuildState) CatchUpRunIDs(ctx context.Context) ([]string, error) {
	rows, err := r.store.readDB().QueryContext(ctx, `
		SELECT DISTINCT run_id FROM change
		WHERE seq > ? AND run_id IS NOT NULL
		ORDER BY run_id`, r.RebuildFromSeq)
	if err != nil {
		return nil, fmt.Errorf("readmodel: read catch-up ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return nil, fmt.Errorf("readmodel: scan catch-up id: %w", err)
		}
		out = append(out, runID)
	}
	return out, rows.Err()
}

// Validate checks the new epoch before it is allowed to replace anything.
//
// The regression check is the load-bearing one: if any run's projected source
// position is BEHIND the old epoch's, publishing the new one would move that run
// backwards in time for every client. That aborts the swap rather than
// publishing, because a rebuild that loses ground is worse than no rebuild.
func (r *RebuildState) Validate(ctx context.Context) error {
	newState, err := r.next.State(ctx)
	if err != nil {
		return err
	}
	oldState, err := r.store.State(ctx)
	if err != nil {
		return err
	}
	if newState.SchemaVersion != oldState.SchemaVersion {
		return fmt.Errorf("readmodel: rebuild schema version %d does not match live %d",
			newState.SchemaVersion, oldState.SchemaVersion)
	}
	if newState.Epoch == oldState.Epoch {
		return errors.New("readmodel: rebuild produced the live epoch; a client could not " +
			"tell the two stores apart")
	}
	return r.assertNoRegression(ctx)
}

// assertNoRegression compares projected source positions run by run.
func (r *RebuildState) assertNoRegression(ctx context.Context) error {
	rows, err := r.store.readDB().QueryContext(ctx,
		`SELECT run_id, last_seq FROM run ORDER BY run_id`)
	if err != nil {
		return fmt.Errorf("readmodel: read live positions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			runID   string
			liveSeq uint64
		)
		if err := rows.Scan(&runID, &liveSeq); err != nil {
			return fmt.Errorf("readmodel: scan live position: %w", err)
		}
		var rebuiltSeq uint64
		err := r.next.readDB().QueryRowContext(ctx,
			`SELECT last_seq FROM run WHERE run_id = ?`, runID).Scan(&rebuiltSeq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Absent from the rebuild. Legitimate only if it was deliberately
			// aged out; otherwise the rebuild would silently drop a run.
			tombstoned, terr := r.next.Tombstoned(ctx, runID)
			if terr != nil {
				return terr
			}
			if !tombstoned {
				return fmt.Errorf("readmodel: rebuild is missing run %s, which the live "+
					"epoch holds at source position %d and which is not tombstoned",
					runID, liveSeq)
			}
		case err != nil:
			return fmt.Errorf("readmodel: read rebuilt position for %s: %w", runID, err)
		case rebuiltSeq < liveSeq:
			return fmt.Errorf("readmodel: rebuild would move run %s backwards, from source "+
				"position %d to %d; publishing it would rewind that run for every client",
				runID, liveSeq, rebuiltSeq)
		}
	}
	return rows.Err()
}

// Abort discards the in-progress build.
//
// Safe at any point: the live store was never modified, and the only artefact is
// the half-built file this removes.
func (r *RebuildState) Abort() error {
	if r.next != nil {
		_ = r.next.Close()
		r.next = nil
	}
	return removeDatabaseFiles(r.Path)
}

// removeDatabaseFiles removes a SQLite database and its WAL sidecars.
//
// All three, because leaving a -wal behind next to a removed main file makes the
// next Open of that path recover a database out of the orphaned log — which is
// how an aborted rebuild's rows would reappear inside the next one.
func removeDatabaseFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("readmodel: remove %s%s: %w", path, suffix, err)
		}
	}
	return nil
}

// setEpoch stamps an epoch onto a freshly opened store.
func (s *Store) setEpoch(ctx context.Context, epoch string) error {
	if _, err := s.writeDB().ExecContext(ctx,
		`UPDATE projection_state SET epoch = ? WHERE id = 1`, epoch); err != nil {
		return fmt.Errorf("readmodel: set epoch: %w", err)
	}
	return nil
}

// StaleRebuildFiles lists half-built epoch files left beside a store.
//
// Startup recovery uses this. §6.5 requires the change-retention pin to release
// on EVERY terminal outcome — success, abort, discard, and an orphan found at
// startup. Without the last one, a rebuild killed mid-flight blocks change
// pruning indefinitely, and the feed grows without bound for a reason nobody is
// looking at.
func StaleRebuildFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("readmodel: scan %s for stale rebuilds: %w", dir, err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || len(name) < len("read-.db") {
			continue
		}
		if name[:len("read-")] == "read-" && filepath.Ext(name) == ".db" {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// DiscardStaleRebuilds removes orphaned rebuild files.
func DiscardStaleRebuilds(dir string) (int, error) {
	files, err := StaleRebuildFiles(dir)
	if err != nil {
		return 0, err
	}
	for _, file := range files {
		if err := removeDatabaseFiles(file); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}

// Swap replaces the live store's file with the rebuilt epoch, in place.
//
// # Why this is not a rename
//
// A rename alone does not work for a WAL database with pooled readers:
//
//   - the `-wal` and `-shm` sidecars cannot be atomically swapped with the main
//     file by one rename, so a reader can observe a new main file beside an old
//     log;
//   - on Unix, connections already in the pool keep reading the OLD inode after
//     the rename, so the swap appears to succeed while every pooled reader
//     serves stale data indefinitely;
//   - on Windows, replacing a file another handle has open generally fails.
//
// So the pool is CLOSED first, then the files are moved, then the handles are
// reopened against the new inode. Requests during that window fail closed with
// 503 + Retry-After rather than being served a stale inode — a brief, visible
// refusal instead of silent wrongness.
//
// The previous epoch is retained under a `.previous` suffix until the reopen is
// confirmed, and only then removed. If the reopen fails, it is moved back.
func (r *RebuildState) Swap(ctx context.Context) error {
	if r.next == nil {
		return errors.New("readmodel: rebuild has no target; it was aborted or already swapped")
	}
	if err := r.Validate(ctx); err != nil {
		return err
	}

	live := r.store.path
	previous := live + ".previous"

	// Close BOTH stores before touching any file. The rebuild target's own WAL
	// has to be checkpointed into its main file, or moving the main file alone
	// would leave its recent commits behind in a log we are about to orphan.
	if err := r.next.Close(); err != nil {
		return fmt.Errorf("readmodel: close rebuild target: %w", err)
	}
	r.next = nil
	if err := r.store.closeHandles(); err != nil {
		return fmt.Errorf("readmodel: quiesce live store: %w", err)
	}

	if err := os.Rename(live, previous); err != nil && !os.IsNotExist(err) {
		// Nothing has moved yet, so reopening the live store restores service.
		_ = r.store.reopen()
		return fmt.Errorf("readmodel: retain previous epoch: %w", err)
	}
	if err := os.Rename(r.Path, live); err != nil {
		// Put the previous epoch back and reopen it. A failed swap must leave the
		// old store serving, not leave the instance with no read model at all.
		_ = os.Rename(previous, live)
		_ = r.store.reopen()
		return fmt.Errorf("readmodel: publish rebuilt epoch: %w", err)
	}
	// The old sidecars belong to the old inode and must not be inherited by the
	// new file, which has its own.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(live + suffix)
		if err := os.Rename(r.Path+suffix, live+suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("readmodel: move %s sidecar: %w", suffix, err)
		}
	}

	if err := r.store.reopen(); err != nil {
		// The new epoch is in place but unopenable. Roll back to the previous
		// file rather than leaving the instance with a store it cannot read.
		_ = os.Remove(live)
		_ = os.Rename(previous, live)
		_ = r.store.reopen()
		return fmt.Errorf("readmodel: reopen against rebuilt epoch: %w", err)
	}

	// Confirmed. Only now is the previous epoch removed.
	return removeDatabaseFiles(previous)
}

// closeHandles closes the writer and reader pool without discarding the store.
func (s *Store) closeHandles() error {
	var readerErr error
	if s.reader != nil {
		readerErr = s.reader.Close()
		s.reader = nil
	}
	var writerErr error
	if s.writer != nil {
		writerErr = s.writeDB().Close()
		s.writer = nil
	}
	return errors.Join(readerErr, writerErr)
}

// reopen re-establishes handles against whatever file is now at the store's
// path.
func (s *Store) reopen() error {
	writer, err := sql.Open("sqlite", s.path+dsnParams)
	if err != nil {
		return fmt.Errorf("readmodel: reopen %s: %w", s.path, err)
	}
	// One connection, matching Open: the first-open WAL switch and any migration
	// must stay single-threaded, and after a swap the new file may be at a
	// different schema version than the handle that closed.
	writer.SetMaxOpenConns(1)
	s.writer = writer
	if err := s.migrate(context.Background()); err != nil {
		return err
	}
	s.reader = openReaderPool(s.path)
	return nil
}
