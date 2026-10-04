package triggerqueue

import (
	"context"
	"errors"
)

// ByKey is the host's internal idempotency lookup. Callers must compare actor
// and request before returning any record; this is not a user-facing lookup.
func (s *Store) ByKey(ctx context.Context, key string) (Record, error) {
	return scanRecord(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE key=?", key))
}

// RetainedPage inventories unacknowledged starts before generation pruning.
// A dispatched receipt has a published journal, whose existing retention owns
// its generation. A dispatching receipt still protects its accepted pins.
// Demand IDs sort before starts so transferring an obligation between pages
// cannot move its dependency behind the inventory cursor.
func (s *Store) RetainedPage(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid retained page limit")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM (SELECT "+columns+" FROM triggers WHERE state IN ('accepted','dispatching') OR EXISTS(SELECT 1 FROM start_controls sc WHERE sc.acceptance_id=triggers.id AND sc.cancel_ns IS NOT NULL AND sc.disposition='' AND sc.cancel_outcome='') UNION ALL SELECT 'demand-'||id AS id,id AS key,'scheduler' AS actor,payload,'accepted' AS state,'' AS run_id,'' AS reason,accepted_ns FROM schedule_demands) WHERE id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
