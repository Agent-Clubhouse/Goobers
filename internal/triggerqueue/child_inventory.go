package triggerqueue

import (
	"context"
	"errors"
)

// IsChildParent distinguishes existing bounded family custody from an ordinary
// run. It is not an authorization or cancellation check.
func (s *Store) IsChildParent(ctx context.Context, parent ChildParent) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM child_parents WHERE gaggle=? AND parent_run=?)`, parent.Gaggle, parent.ParentRunID).Scan(&exists)
	return exists, err
}

// RetainedChildStarts enumerates full source-custody owners, including terminal
// children awaiting acknowledgement. Tombstones have released source custody.
// Use the last returned ID as the next cursor. This is internal recovery data,
// never a cross-gaggle portal or tool lookup.
func (s *Store) RetainedChildStarts(ctx context.Context, after string, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: batch limit must be 1..100")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.key,t.actor,t.payload,t.state,t.run_id,t.reason,t.accepted_ns FROM triggers t JOIN child_lineages c ON c.acceptance_id=t.id WHERE c.tombstoned_ns IS NULL AND t.id>? ORDER BY t.id LIMIT ?`, after, limit)
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

// UnsettledChildParents includes stages that obtained authority but never made
// a child. The bounded lifecycle sweep closes only durably completed/cancelled
// families; human-waiting families remain open and capacity bounded.
func (s *Store) UnsettledChildParents(ctx context.Context, after ChildParent, limit int) ([]ChildParent, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: batch limit must be 1..100")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT gaggle,parent_run FROM child_parents WHERE settled_ns IS NULL AND (gaggle>? OR (gaggle=? AND parent_run>?)) ORDER BY gaggle,parent_run LIMIT ?`, after.Gaggle, after.Gaggle, after.ParentRunID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var parents []ChildParent
	for rows.Next() {
		var parent ChildParent
		if err := rows.Scan(&parent.Gaggle, &parent.ParentRunID); err != nil {
			return nil, err
		}
		parents = append(parents, parent)
	}
	return parents, rows.Err()
}
