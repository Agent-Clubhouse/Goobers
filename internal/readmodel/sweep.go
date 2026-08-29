package readmodel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Sweep state: the repair cursor, the unpublished memo, and tombstones
// (#1924, design §6.3).

// SweepCursor is a resumable position in the repair walk.
//
// Durable because the bound depends on it. A fixed I/O budget only produces a
// complete cycle if the walk can stop anywhere and resume; a cursor held only in
// memory would restart from the beginning on every daemon restart, and on a
// frequently-restarted instance the tail of the corpus would never be swept.
type SweepCursor struct {
	Root                  string
	AfterName             string
	CycleStartedAt        time.Time
	LastCycleCompletedAt  time.Time
	EntriesThisCycle      int
	ReverseAfterStartedAt time.Time
	ReverseAfterRunID     string
	ReverseCycleBefore    time.Time
	ForwardNext           bool
}

// SweepCursor reads the repair walk's position.
func (s *Store) SweepCursor(ctx context.Context) (SweepCursor, error) {
	var (
		cursor        SweepCursor
		started       sql.NullString
		completed     sql.NullString
		reverseAfter  sql.NullString
		reverseBefore sql.NullString
	)
	db, release, err := s.readHandle()
	if err != nil {
		return SweepCursor{}, err
	}
	defer release()
	err = db.QueryRowContext(ctx, `
		SELECT root, after_name, cycle_started_at, last_cycle_completed_at, entries_this_cycle,
		       reverse_after_started_at, reverse_after_run_id, reverse_cycle_before,
		       forward_next
		FROM sweep_cursor WHERE id = 1`).
		Scan(
			&cursor.Root,
			&cursor.AfterName,
			&started,
			&completed,
			&cursor.EntriesThisCycle,
			&reverseAfter,
			&cursor.ReverseAfterRunID,
			&reverseBefore,
			&cursor.ForwardNext,
		)
	if errors.Is(err, sql.ErrNoRows) {
		return SweepCursor{}, nil
	}
	if err != nil {
		return SweepCursor{}, fmt.Errorf("readmodel: read sweep cursor: %w", err)
	}
	if cursor.CycleStartedAt, err = optionalTimeValue(started); err != nil {
		return SweepCursor{}, err
	}
	if cursor.LastCycleCompletedAt, err = optionalTimeValue(completed); err != nil {
		return SweepCursor{}, err
	}
	if cursor.ReverseAfterStartedAt, err = optionalTimeValue(reverseAfter); err != nil {
		return SweepCursor{}, err
	}
	if cursor.ReverseCycleBefore, err = optionalTimeValue(reverseBefore); err != nil {
		return SweepCursor{}, err
	}
	return cursor, nil
}

// SaveSweepCursor records the walk's position.
func (s *Store) SaveSweepCursor(ctx context.Context, cursor SweepCursor) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	_, err = db.ExecContext(ctx, `
		UPDATE sweep_cursor SET
			root = ?, after_name = ?,
			cycle_started_at = ?, last_cycle_completed_at = ?,
			entries_this_cycle = ?,
			reverse_after_started_at = ?, reverse_after_run_id = ?,
			reverse_cycle_before = ?, forward_next = ?
		WHERE id = 1`,
		cursor.Root, cursor.AfterName,
		nullTimeValue(cursor.CycleStartedAt), nullTimeValue(cursor.LastCycleCompletedAt),
		cursor.EntriesThisCycle,
		nullTimeValue(cursor.ReverseAfterStartedAt), cursor.ReverseAfterRunID,
		nullTimeValue(cursor.ReverseCycleBefore), cursor.ForwardNext)
	if err != nil {
		return fmt.Errorf("readmodel: save sweep cursor: %w", err)
	}
	return nil
}

// ProjectionFloor reports the point below which runs are intentionally aged out.
func (s *Store) ProjectionFloor(ctx context.Context) (time.Time, bool, error) {
	var floor sql.NullString
	db, release, err := s.readHandle()
	if err != nil {
		return time.Time{}, false, err
	}
	defer release()
	err = db.QueryRowContext(ctx,
		`SELECT projection_floor FROM projection_state WHERE id = 1`).Scan(&floor)
	// The error is checked BEFORE floor.Valid, and the two are separate
	// conditions rather than one `||`.
	//
	// They were combined, and the short-circuit swallowed every query failure as
	// "no floor is set": on any error, floor stays invalid, `!floor.Valid` is
	// true, and the caller is told there is no floor with a nil error. Repair
	// would then re-admit every aged-out run, retention would delete them again,
	// and the livelock the floor exists to prevent runs on a transient database
	// error.
	//
	// Found by a test asserting reads fail after Close — the closed handle
	// produced exactly this silent "no floor".
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("readmodel: read projection floor: %w", err)
	}
	if !floor.Valid {
		return time.Time{}, false, nil
	}
	parsed, err := requiredTime(floor)
	if err != nil {
		return time.Time{}, false, err
	}
	return parsed, true, nil
}

// SetProjectionFloor raises the floor.
//
// It only ever advances. A floor that moved backwards would re-admit runs that
// were deliberately aged out, which is the livelock the floor exists to prevent
// — repair projects, retention deletes, and the cycle repeats forever.
func (s *Store) SetProjectionFloor(ctx context.Context, floor time.Time) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	_, err = db.ExecContext(ctx, `
		UPDATE projection_state
		SET projection_floor = ?
		WHERE id = 1 AND (projection_floor IS NULL OR projection_floor < ?)`,
		formatTime(floor), formatTime(floor))
	if err != nil {
		return fmt.Errorf("readmodel: set projection floor: %w", err)
	}
	return nil
}

// IsUnpublished reports whether a directory is remembered as having no run.yaml
// AT THIS MTIME.
//
// The mtime comparison is the whole mechanism. 10,906 of 40,665 directories on
// the live instance are unpublished and can never be ingested; remembering them
// makes each cost one stat per cycle. Keying on mtime is what keeps that from
// becoming permanent: writing run.yaml bumps the directory's mtime, so a
// promoted run no longer matches its memo and is examined again.
func (s *Store) IsUnpublished(ctx context.Context, runID string, mtime time.Time) (bool, error) {
	var recorded string
	db, release, err := s.readHandle()
	if err != nil {
		return false, err
	}
	defer release()
	err = db.QueryRowContext(ctx,
		`SELECT dir_mtime FROM unpublished WHERE run_id = ?`, runID).Scan(&recorded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("readmodel: read unpublished %s: %w", runID, err)
	}
	return recorded == formatTime(mtime), nil
}

// MarkUnpublished remembers a directory as carrying no run.yaml.
func (s *Store) MarkUnpublished(ctx context.Context, runID string, mtime time.Time) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	_, err = db.ExecContext(ctx, `
		INSERT INTO unpublished (run_id, dir_mtime, seen_at) VALUES (?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET dir_mtime = excluded.dir_mtime, seen_at = excluded.seen_at`,
		runID, formatTime(mtime), formatTime(s.now()))
	if err != nil {
		return fmt.Errorf("readmodel: mark %s unpublished: %w", runID, err)
	}
	return nil
}

// ClearUnpublished forgets a directory's unpublished memo.
func (s *Store) ClearUnpublished(ctx context.Context, runID string) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	if _, err := db.ExecContext(ctx,
		`DELETE FROM unpublished WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("readmodel: clear unpublished %s: %w", runID, err)
	}
	return nil
}

// Tombstoned reports whether a run was deliberately aged out.
func (s *Store) Tombstoned(ctx context.Context, runID string) (bool, error) {
	var one int
	db, release, err := s.readHandle()
	if err != nil {
		return false, err
	}
	defer release()
	err = db.QueryRowContext(ctx,
		`SELECT 1 FROM tombstone WHERE run_id = ?`, runID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("readmodel: read tombstone %s: %w", runID, err)
	}
	return true, nil
}

// Tombstone records that a run was deliberately aged out.
//
// The reason is stored because "missing" and "deliberately gone" are different
// answers to an operator's question, and a tombstone with no reason collapses
// them again one level down.
func (s *Store) Tombstone(ctx context.Context, runID string, startedAt time.Time, reason string) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	_, err = db.ExecContext(ctx, `
		INSERT INTO tombstone (run_id, started_at, tombstoned_at, reason) VALUES (?, ?, ?, ?)
		ON CONFLICT(run_id) DO NOTHING`,
		runID, formatTime(startedAt), formatTime(s.now()), reason)
	if err != nil {
		return fmt.Errorf("readmodel: tombstone %s: %w", runID, err)
	}
	return nil
}

// ProjectedRunIDsBefore returns projected runs for the reverse sweep.
//
// Ordered oldest-first so the reverse direction makes progress through history
// rather than re-examining the newest rows every step — the newest are also the
// ones least likely to have lost their journal.
func (s *Store) ProjectedRunIDsBefore(ctx context.Context, before time.Time, limit int) ([]RunRow, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}

	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, `
		SELECT run_id, started_at FROM run
		WHERE started_at <= ?
		ORDER BY started_at ASC, run_id ASC
		LIMIT ?`, formatTime(before), limit)
	if err != nil {
		return nil, fmt.Errorf("readmodel: read projected runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunRow
	for rows.Next() {
		var (
			row       RunRow
			startedAt string
		)
		if err := rows.Scan(&row.RunID, &startedAt); err != nil {
			return nil, fmt.Errorf("readmodel: scan projected run: %w", err)
		}
		parsed, err := time.Parse(timeFormat, startedAt)
		if err != nil {
			return nil, fmt.Errorf("readmodel: parse started_at %q: %w", startedAt, err)
		}
		row.StartedAt = parsed
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: projected run rows: %w", err)
	}
	return out, nil
}

// ProjectedRunIDsAfter returns one keyset page for the repair reverse sweep.
func (s *Store) ProjectedRunIDsAfter(
	ctx context.Context,
	afterStartedAt time.Time,
	afterRunID string,
	before time.Time,
	limit int,
) ([]RunRow, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, `
		SELECT run_id, started_at FROM run
		WHERE started_at <= ?
		  AND (? = '' OR started_at > ? OR (started_at = ? AND run_id > ?))
		ORDER BY started_at ASC, run_id ASC
		LIMIT ?`,
		formatTime(before),
		afterRunID,
		formatTime(afterStartedAt),
		formatTime(afterStartedAt),
		afterRunID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("readmodel: read projected runs after cursor: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunRow
	for rows.Next() {
		var (
			row       RunRow
			startedAt string
		)
		if err := rows.Scan(&row.RunID, &startedAt); err != nil {
			return nil, fmt.Errorf("readmodel: scan projected run after cursor: %w", err)
		}
		parsed, err := time.Parse(timeFormat, startedAt)
		if err != nil {
			return nil, fmt.Errorf("readmodel: parse started_at %q: %w", startedAt, err)
		}
		row.StartedAt = parsed
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: projected run rows after cursor: %w", err)
	}
	return out, nil
}

// optionalTimeValue parses a nullable timestamp into a zero-able time.
func optionalTimeValue(value sql.NullString) (time.Time, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(timeFormat, value.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("readmodel: parse time %q: %w", value.String, err)
	}
	return parsed, nil
}
