package triggerqueue

import (
	"context"
	"errors"
)

// EventRoutingScopes rotates bounded host routing work across gaggle partitions.
func (s *Store) EventRoutingScopes(ctx context.Context, after string, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid event scope page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT gaggle FROM (SELECT gaggle FROM event_receipts WHERE state='routing_pending' UNION SELECT gaggle FROM event_groups WHERE state='open') WHERE gaggle>? ORDER BY gaggle LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []string
	for rows.Next() {
		var gaggle string
		if err = rows.Scan(&gaggle); err != nil {
			return nil, err
		}
		result = append(result, gaggle)
	}
	return result, rows.Err()
}

// UnsettledEventGroups includes dispatched consumers: queue acknowledgment does
// not end input custody, and human-paused executions remain in this inventory.
func (s *Store) UnsettledEventGroups(ctx context.Context, after string, limit int) ([]EventGroup, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid event execution page")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+eventGroupColumns+" FROM event_groups WHERE state='queued' AND settled_ns IS NULL AND id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EventGroup
	for rows.Next() {
		group, err := scanEventGroup(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, group)
	}
	return result, rows.Err()
}
