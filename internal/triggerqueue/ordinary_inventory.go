package triggerqueue

import (
	"context"
	"errors"
)

// ByKey returns internal acceptance custody. Callers must verify the recorded
// actor and request before exposing an idempotent response.
func (s *Store) ByKey(ctx context.Context, key string) (Record, error) {
	return scanRecord(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE key=?", key))
}

// RetainedPage enumerates accepted or uncertain execution owners for generation
// retention. Confirmed dispatched runs retain their archive through the journal.
// It is an internal recovery query, not a cross-gaggle browsing surface.
func (s *Store) RetainedPage(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: batch limit must be 1..100")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM triggers WHERE state IN ('accepted','dispatching') AND id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}
