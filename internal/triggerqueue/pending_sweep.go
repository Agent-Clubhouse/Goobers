package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
)

const pendingSweepSchema = `CREATE TABLE pending_start_sweep (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1), accepted_ns INTEGER NOT NULL,
 last_id TEXT NOT NULL CHECK(length(CAST(last_id AS BLOB))<=128)
);
INSERT INTO pending_start_sweep(singleton,accepted_ns,last_id) VALUES(1,0,'');
CREATE INDEX triggers_pending_position ON triggers(accepted_ns,id) WHERE state='accepted';`

// PendingSweep reserves the next bounded inspection window transactionally.
// Reservation advances only enumeration: records remain accepted and dispatch
// still claims each one separately. A crash before inspection delays untouched
// records until the cursor wraps; it cannot drop them or authorize a duplicate.
// The shared cursor survives reopen and cannot be pinned by a held prefix.
func (s *Store) PendingSweep(ctx context.Context, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: batch limit must be 1..100")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var ns int64
	var id string
	if err = tx.QueryRowContext(ctx, `SELECT accepted_ns,last_id FROM pending_start_sweep WHERE singleton=1`).Scan(&ns, &id); err != nil {
		return nil, err
	}
	records, err := pendingSweepWindow(ctx, tx, ns, id, limit)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 && id != "" {
		records, err = pendingSweepWindow(ctx, tx, 0, "", limit)
		if err != nil {
			return nil, err
		}
	}
	ns, id = 0, ""
	if len(records) > 0 {
		last := records[len(records)-1]
		ns, id = last.AcceptedAt.UnixNano(), last.ID
	}
	if _, err = tx.ExecContext(ctx, `UPDATE pending_start_sweep SET accepted_ns=?,last_id=? WHERE singleton=1 AND (accepted_ns<>? OR last_id<>?)`, ns, id, ns, id); err != nil {
		return nil, err
	}
	return records, tx.Commit()
}

func pendingSweepWindow(ctx context.Context, tx *sql.Tx, ns int64, id string, limit int) ([]Record, error) {
	query := "SELECT " + columns + " FROM triggers WHERE state='accepted' AND NOT EXISTS(SELECT 1 FROM direct_engine_inputs i WHERE i.acceptance_id=triggers.id)"
	var args []any
	if id != "" {
		// A tuple range seeks directly into the pending-order index. An OR
		// against an empty-cursor sentinel would rescan the prefix every time.
		query += " AND (accepted_ns,id)>(?,?)"
		args = append(args, ns, id)
	}
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, query+" ORDER BY accepted_ns,id LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
