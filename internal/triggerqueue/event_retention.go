package triggerqueue

import (
	"context"
	"errors"
	"time"
)

// EventPruneResult counts bounded work units sharing one maintenance budget.
type EventPruneResult struct{ Expired, Tombstoned, Deleted int }

// PruneEvents runs in the daemon sweep, including while the scheduler is absent.
// Pending routing has an explicit one-hour custody deadline; consumer execution
// is a separate dependency and must be retained by its delivery owner. Completed
// unmatched/failed receipts keep payloads for seven days, then identity/digest for
// another thirty days. A full tombstone quota never evicts a dedup promise.
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
 SELECT id FROM event_receipts WHERE tombstoned_ns<=? ORDER BY tombstoned_ns,id LIMIT ?)`, now.Add(-EventTombstoneRetention).UnixNano(), limit)
	if err != nil {
		return fail(err)
	}
	count, err := deleted.RowsAffected()
	if err != nil {
		return fail(err)
	}
	result.Deleted = int(count)
	expired, err := tx.ExecContext(ctx, `UPDATE event_receipts SET state='routing_failed', reason='routing deadline exceeded',finished_ns=?,reserved_bytes=0 WHERE id IN (
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
 AND (SELECT COUNT(*) FROM event_receipts e WHERE e.gaggle=r.gaggle AND e.tombstoned_ns IS NOT NULL)<?
 ORDER BY r.finished_ns,r.id LIMIT ?`, now.Add(-EventRetention).UnixNano(), MaxEventTombstones, limit-result.Deleted-result.Expired)
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
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return result, nil
}
