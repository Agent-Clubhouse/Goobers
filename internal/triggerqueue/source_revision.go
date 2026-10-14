package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const sourceRevisionSchema = `ALTER TABLE source_start_cursors ADD COLUMN revision TEXT NOT NULL DEFAULT '';`

// SourceCursorRevision adopts an unversioned cursor once, preserving its pending
// fire. A subsequent schedule edit resets future evaluation at observation time;
// accepted starts and demand obligations have independent, unchanged custody.
func (s *Store) SourceCursorRevision(ctx context.Context, scope, revision string, initial, observed time.Time, legacy bool) (time.Time, bool, error) {
	if !validChildText(scope, 256, true) || !validChildText(revision, 128, true) || initial.IsZero() || observed.IsZero() {
		return time.Time{}, false, errors.New("triggerqueue: invalid source revision")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var ns int64
	var pending bool
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT cursor_ns,legacy_pending,revision FROM source_start_cursors WHERE scope=?`, scope).Scan(&ns, &pending, &prior)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err = triggerSlotCapacity(ctx, tx, 1); err != nil {
			return time.Time{}, false, err
		}
		if err = childByteCapacity(ctx, tx, len(scope)+len(revision)+4096); err != nil {
			return time.Time{}, false, err
		}
		ns, pending = initial.UnixNano(), legacy
		_, err = tx.ExecContext(ctx, `INSERT INTO source_start_cursors(scope,cursor_ns,legacy_pending,revision) VALUES(?,?,?,?)`, scope, ns, pending, revision)
	case err != nil:
		return time.Time{}, false, err
	case prior == revision:
		return time.Unix(0, ns).UTC(), pending, nil
	case prior == "":
		_, err = tx.ExecContext(ctx, `UPDATE source_start_cursors SET revision=? WHERE scope=?`, revision, scope)
	default:
		ns, pending = observed.UnixNano(), false
		_, err = tx.ExecContext(ctx, `UPDATE source_start_cursors SET revision=?,cursor_ns=?,legacy_pending=0 WHERE scope=?`, revision, ns, scope)
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(0, ns).UTC(), pending, tx.Commit()
}
