package triggerqueue

import (
	"context"
	"time"
)

// PrunePRRepairCommands performs at most limit bounded row transitions. Only
// confirmed/not-applied results become compact tombstones after 30 days; the
// tombstone then denies reexecution for another 30 days. After this explicit
// 60-day guarantee, a new request must pass current target, identity and revision
// preflight. Accepted, attempting and unknown custody never ages out or yields
// capacity to new commands. No provider call is made by maintenance.
func (s *Store) PrunePRRepairCommands(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return 0, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := now.Add(-WorkbenchCommandRetention).UnixNano()
	ids, err := childPruneIDs(ctx, tx, `SELECT id FROM pr_repair_commands WHERE state='tombstoned' AND tombstoned_ns<=? ORDER BY id LIMIT ?`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM pr_repair_commands WHERE id=? AND state='tombstoned'`, id); err != nil {
			return 0, err
		}
	}
	count := len(ids)
	ids, err = childPruneIDs(ctx, tx, `SELECT id FROM pr_repair_commands WHERE state IN ('confirmed','not-applied') AND completed_ns<=? AND tombstoned_ns IS NULL AND NOT EXISTS(SELECT 1 FROM pr_repair_commands dependent WHERE dependent.parent_id=pr_repair_commands.id) ORDER BY id LIMIT ?`, cutoff, limit-count)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE pr_repair_commands SET state='tombstoned',request='',evidence='',receipt='',reserved_bytes=0,turn_id=NULL,parent_id=NULL,tombstoned_ns=? WHERE id=?`, now.UnixNano(), id); err != nil {
			return 0, err
		}
	}
	return count + len(ids), tx.Commit()
}
