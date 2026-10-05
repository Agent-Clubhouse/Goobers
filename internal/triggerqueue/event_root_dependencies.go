package triggerqueue

import (
	"context"
	"errors"
)

// EventRootDependency retains a workflow producer whose root budget cannot be
// released merely because its event receipts have aged out. The whole value is
// the stable page cursor; an empty value starts the inventory.
type EventRootDependency struct{ Gaggle, RootID, SourceRunID string }

// EventRootDependencyPage supplements EventDependencyPage with workflow root
// custody. The host must complete both inventories before pruning journals.
// Releasing these references requires proof that every producer and descendant
// of the root settled; that host acknowledgment is not implemented yet.
func (s *Store) EventRootDependencyPage(ctx context.Context, after EventRootDependency, limit int) ([]EventRootDependency, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid event root dependency page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT gaggle,root_id,source_run_id FROM event_root_sources
 WHERE (gaggle,root_id,source_run_id)>(?,?,?) ORDER BY gaggle,root_id,source_run_id LIMIT ?`, after.Gaggle, after.RootID, after.SourceRunID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EventRootDependency
	for rows.Next() {
		var pin EventRootDependency
		if err := rows.Scan(&pin.Gaggle, &pin.RootID, &pin.SourceRunID); err != nil {
			return nil, err
		}
		result = append(result, pin)
	}
	return result, rows.Err()
}
