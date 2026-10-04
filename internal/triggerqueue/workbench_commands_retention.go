package triggerqueue

import (
	"context"
	"time"
)

// PruneWorkbenchCommands performs at most limit bounded row transitions. Only
// confirmed/observed/not-applied results become compact tombstones after 30 days; the
// tombstone then denies reexecution for another 30 days. After this explicit
// 60-day guarantee, a new request must pass current target, identity and revision
// preflight. Accepted, attempting and unknown custody never ages out or yields
// capacity to new commands. No provider call is made by maintenance.
func (s *Store) PruneWorkbenchCommands(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return 0, ErrTransition
	}
	pruners := []func(context.Context, time.Time, int) (int, error){s.pruneNativeWorkbenchCommands, s.PruneWorkbenchProposals, s.PruneNeedsHumanCommands, s.PrunePRRepairCommands, s.PruneWorkbenchSuggestions}
	count := 0
	for i, prune := range pruners {
		remaining := limit - count
		if remaining == 0 {
			return count, nil
		}
		// Each kind gets a share before a busy earlier kind can consume the budget.
		allowance := (remaining + len(pruners) - i - 1) / (len(pruners) - i)
		n, err := prune(ctx, now, allowance)
		count += n
		if err != nil {
			return count, err
		}
	}
	return count, nil
}

func (s *Store) pruneNativeWorkbenchCommands(ctx context.Context, now time.Time, limit int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := now.Add(-WorkbenchCommandRetention).UnixNano()
	ids, err := childPruneIDs(ctx, tx, `SELECT id FROM workbench_commands WHERE state='tombstoned' AND tombstoned_ns<=? ORDER BY id LIMIT ?`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM workbench_commands WHERE id=? AND state='tombstoned'`, id); err != nil {
			return 0, err
		}
	}
	count := len(ids)
	ids, err = childPruneIDs(ctx, tx, `SELECT id FROM workbench_commands WHERE state IN ('confirmed','not-applied') AND completed_ns<=? AND tombstoned_ns IS NULL ORDER BY id LIMIT ?`, cutoff, limit-count)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE workbench_commands SET state='tombstoned',request='',receipt='',reserved_bytes=0,tombstoned_ns=? WHERE id=?`, now.UnixNano(), id); err != nil {
			return 0, err
		}
	}
	return count + len(ids), tx.Commit()
}
