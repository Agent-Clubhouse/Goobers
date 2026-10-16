package triggerqueue

import (
	"context"
	"fmt"
)

// CurrentChild returns the one unresolved occurrence slot. The bounded query
// detects corrupt duplicate ownership rather than choosing an arbitrary child.
// This is an internal host API; its selectors do not authorize tool access.
func (s *Store) CurrentChild(ctx context.Context, parent ChildParent, occurrence string) (ChildRecord, error) {
	if !parent.valid() || !validChildText(occurrence, 256, true) {
		return ChildRecord{}, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+childColumns+childFrom+` WHERE c.gaggle=? AND c.parent_run=? AND c.occurrence=? AND c.acknowledged_ns IS NULL AND c.tombstoned_ns IS NULL ORDER BY c.sequence LIMIT 2`, parent.Gaggle, parent.ParentRunID, occurrence)
	if err != nil {
		return ChildRecord{}, err
	}
	defer func() { _ = rows.Close() }()
	var result ChildRecord
	if rows.Next() {
		result, err = scanChild(rows)
		if err != nil {
			return ChildRecord{}, err
		}
	}
	if rows.Next() {
		return ChildRecord{}, fmt.Errorf("triggerqueue: child occurrence has multiple unresolved owners")
	}
	return result, rows.Err()
}
