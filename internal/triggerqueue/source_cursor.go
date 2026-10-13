package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const sourceCursorSchema = `CREATE TABLE source_start_cursors (scope TEXT PRIMARY KEY NOT NULL, cursor_ns INTEGER NOT NULL, legacy_pending INTEGER NOT NULL DEFAULT 0);`

// SourceCursorWithLegacy adopts an outstanding legacy fire only when creating
// this cursor. Later legacy file replays cannot resurrect a transferred fire.
func (s *Store) SourceCursorWithLegacy(ctx context.Context, scope string, initial time.Time, legacy bool) (time.Time, bool, error) {
	if !validChildText(scope, 256, true) || initial.IsZero() {
		return time.Time{}, false, errors.New("triggerqueue: invalid source cursor")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var ns int64
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT cursor_ns,legacy_pending FROM source_start_cursors WHERE scope=?`, scope).Scan(&ns, &pending)
	if err == nil {
		return time.Unix(0, ns).UTC(), pending, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, err
	}
	if err = triggerSlotCapacity(ctx, tx, 1); err != nil {
		return time.Time{}, false, err
	}
	if err = childByteCapacity(ctx, tx, len(scope)+4096); err != nil {
		return time.Time{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO source_start_cursors(scope,cursor_ns,legacy_pending) VALUES(?,?,?)`, scope, initial.UnixNano(), legacy); err != nil {
		return time.Time{}, false, err
	}
	return initial.UTC(), legacy, tx.Commit()
}

func advanceSourceCursor(ctx context.Context, tx *sql.Tx, a *SourceAdvance) error {
	if a == nil {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE source_start_cursors SET cursor_ns=?,legacy_pending=0 WHERE scope=? AND cursor_ns=?`, a.After.UnixNano(), a.Scope, a.Before.UnixNano())
	return changed(result, err)
}
