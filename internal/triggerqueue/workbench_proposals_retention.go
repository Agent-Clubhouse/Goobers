package triggerqueue

import (
	"context"
	"time"
)

// PruneWorkbenchProposals bounds maintenance to limit row transitions. Only
// terminal receipts become tombstones after thirty days, then disappear after
// another thirty. Pending or partially visible effects remain pinned.
func (s *Store) PruneWorkbenchProposals(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return 0, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := now.Add(-WorkbenchCommandRetention).UnixNano()
	ids, err := childPruneIDs(ctx, tx, `SELECT id FROM workbench_proposals WHERE state='tombstoned' AND tombstoned_ns<=? ORDER BY id LIMIT ?`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM workbench_proposals WHERE id=? AND state='tombstoned'`, id); err != nil {
			return 0, err
		}
	}
	count := len(ids)
	ids, err = childPruneIDs(ctx, tx, `SELECT id FROM workbench_proposals WHERE state IN ('confirmed','observed','not-applied') AND completed_ns<=? AND tombstoned_ns IS NULL AND NOT EXISTS(SELECT 1 FROM workbench_suggestions s WHERE s.proposal_id=workbench_proposals.id AND s.tombstoned_ns IS NULL) ORDER BY id LIMIT ?`, cutoff, limit-count)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE workbench_proposals SET state='tombstoned',request='',plan='',before_source='',after_source='',history='',reserved_bytes=0,tombstoned_ns=? WHERE id=?`, now.UnixNano(), id); err != nil {
			return 0, err
		}
	}
	return count + len(ids), tx.Commit()
}
