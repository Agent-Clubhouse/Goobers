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
func (s *Store) RetainedPage(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid retained page limit")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM triggers WHERE id>? AND state IN ('accepted','dispatching') ORDER BY id LIMIT ?", after, limit)
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
