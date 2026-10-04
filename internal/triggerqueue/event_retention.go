package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// EventPruneResult counts bounded work units sharing one maintenance budget.
type EventPruneResult struct{ Expired, Tombstoned, Deleted, GroupsDeleted, RootsDeleted int }

// PruneEvents runs in the daemon sweep, including while the scheduler is absent.
// Pending routing has a one-hour deadline. Routed payloads remain until seven
// days after every consumer settles, then identity/digest and membership history
// remain for another thirty days. Group starts stay pinned through that history.
// A full tombstone quota never evicts a dedup promise. Workflow-associated root
// budgets await a future host acknowledgment that all producers and descendants
// settled; receipt expiry alone cannot reset a long-lived root's start budget.
func (s *Store) PruneEvents(ctx context.Context, now time.Time, limit int) (EventPruneResult, error) {
	var result EventPruneResult
	if now.IsZero() || limit < 1 || limit > 100 {
		return result, errors.New("triggerqueue: invalid event prune batch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	fail := func(err error) (EventPruneResult, error) { return EventPruneResult{}, err }
	deleted, err := tx.ExecContext(ctx, `DELETE FROM event_receipts WHERE id IN (
 SELECT id FROM event_receipts WHERE tombstoned_ns<=? AND NOT EXISTS(SELECT 1 FROM event_outbox o WHERE o.receipt_id=event_receipts.id) ORDER BY tombstoned_ns,id LIMIT ?)`, now.Add(-EventTombstoneRetention).UnixNano(), limit)
	if err != nil {
		return fail(err)
	}
	count, err := deleted.RowsAffected()
	if err != nil {
		return fail(err)
	}
	result.Deleted = int(count)
	expired, err := tx.ExecContext(ctx, `UPDATE event_receipts SET state='routing_failed', reason='routing deadline exceeded',finished_ns=?,reserved_bytes=0,reserved_starts=0 WHERE id IN (
 SELECT id FROM event_receipts WHERE state='routing_pending' AND accepted_ns<=? ORDER BY accepted_ns,id LIMIT ?)`, now.UnixNano(), now.Add(-EventRoutingDeadline).UnixNano(), limit-result.Deleted)
	if err != nil {
		return fail(err)
	}
	count, err = expired.RowsAffected()
	if err != nil {
		return fail(err)
	}
	result.Expired = int(count)
	// Recheck the scope quota for each bounded row; one batch must not exceed it.
	ids, err := childPruneIDs(ctx, tx, `SELECT r.id FROM event_receipts r WHERE r.tombstoned_ns IS NULL AND r.finished_ns<=?
 AND NOT EXISTS(SELECT 1 FROM event_outbox o WHERE o.receipt_id=r.id OR (o.gaggle=r.gaggle AND o.id=r.event_id))
 AND NOT EXISTS(SELECT 1 FROM event_deliveries d JOIN event_groups g ON g.id=d.group_id WHERE d.receipt_id=r.id AND (g.settled_ns IS NULL OR g.settled_ns>?))
 AND (SELECT COUNT(*) FROM event_receipts e WHERE e.gaggle=r.gaggle AND e.tombstoned_ns IS NOT NULL)<?
 ORDER BY r.finished_ns,r.id LIMIT ?`, now.Add(-EventRetention).UnixNano(), now.Add(-EventRetention).UnixNano(), MaxEventTombstones, limit-result.Deleted-result.Expired)
	if err != nil {
		return fail(err)
	}
	for _, id := range ids {
		changed, err := tx.ExecContext(ctx, `UPDATE event_receipts SET tombstoned_ns=?,envelope=X'',plan=X'',reserved_bytes=0
 WHERE id=? AND (SELECT COUNT(*) FROM event_receipts e WHERE e.gaggle=event_receipts.gaggle AND e.tombstoned_ns IS NOT NULL)<?`, now.UnixNano(), id, MaxEventTombstones)
		if err != nil {
			return fail(err)
		}
		count, err := changed.RowsAffected()
		if err != nil {
			return fail(err)
		}
		result.Tombstoned += int(count)
	}
	remaining := limit - result.Deleted - result.Expired - result.Tombstoned
	if result.GroupsDeleted, err = pruneEventGroups(ctx, tx, now, remaining); err != nil {
		return fail(err)
	}
	if result.RootsDeleted, err = pruneEventRoots(ctx, tx, remaining-result.GroupsDeleted); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return result, nil
}

func pruneEventGroups(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	ids, err := childPruneIDs(ctx, tx, `SELECT g.id FROM event_groups g WHERE g.settled_ns<=? AND NOT EXISTS(SELECT 1 FROM event_deliveries d WHERE d.group_id=g.id) ORDER BY g.settled_ns,g.id LIMIT ?`, now.Add(-EventRetention).UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		var acceptance string
		if err = tx.QueryRowContext(ctx, `SELECT acceptance_id FROM event_groups WHERE id=?`, id).Scan(&acceptance); err != nil {
			return 0, err
		}
		for _, query := range []string{`DELETE FROM event_group_roots WHERE group_id=?`, `DELETE FROM event_groups WHERE id=?`} {
			if _, err = tx.ExecContext(ctx, query, id); err != nil {
				return 0, err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM triggers WHERE id=? AND state IN ('dispatched','rejected')`, acceptance); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func pruneEventRoots(ctx context.Context, tx *sql.Tx, limit int) (int, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM event_roots WHERE rowid IN (SELECT r.rowid FROM event_roots r WHERE r.workflow=0 AND
 NOT EXISTS(SELECT 1 FROM event_group_roots g WHERE g.gaggle=r.gaggle AND g.root_id=r.root_id)
 AND NOT EXISTS(SELECT 1 FROM event_receipts e WHERE e.gaggle=r.gaggle AND e.root_id=r.root_id)
 AND NOT EXISTS(SELECT 1 FROM event_receipt_roots e WHERE e.gaggle=r.gaggle AND e.root_id=r.root_id)
 ORDER BY r.gaggle,r.root_id LIMIT ?)`, limit)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}
