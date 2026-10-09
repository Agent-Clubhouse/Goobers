package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ChildPruneResult counts maintenance work units. A lineage tombstone also
// releases its pinned ordinary receipt and potentially its last-owned proposal,
// plus fork/result carriers, one disposition and at most 32 prior choices,
// so one pass deletes at most 38*limit
// physical rows. All stages share the same work budget.
type ChildPruneResult struct {
	Tombstoned         int
	Deleted            int
	OccurrencesDeleted int
	ParentsDeleted     int
	AuthoritiesDeleted int
}

func (r ChildPruneResult) total() int {
	return r.Tombstoned + r.Deleted + r.OccurrencesDeleted + r.ParentsDeleted + r.AuthoritiesDeleted
}

// PruneChildren is bounded daemon maintenance across the internal store, not a
// user-facing read API. Full custody lasts 30 days after both acknowledgement
// and parent settlement. Any unresolved sibling pins the whole family. A
// receipt with uncertain dispatch also pins its full lineage. Digest tombstones
// survive another 30 days; only then can occurrence counters/fences be removed.
// Callers supply a bounded context and repeat passes; no VACUUM or unbounded
// payload scan occurs here. Empty parent fences expire after settlement+30d.
func (s *Store) PruneChildren(ctx context.Context, now time.Time, limit int) (ChildPruneResult, error) {
	var result ChildPruneResult
	if now.IsZero() || limit < 1 || limit > 100 {
		return result, errors.New("triggerqueue: invalid child prune batch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	fail := func(err error) (ChildPruneResult, error) { return ChildPruneResult{}, err }
	if result.Deleted, err = pruneExpiredChildTombstones(ctx, tx, now, limit); err != nil {
		return fail(err)
	}
	if result.Tombstoned, err = tombstoneChildLineages(ctx, tx, now, limit-result.total()); err != nil {
		return fail(err)
	}
	if result.OccurrencesDeleted, err = pruneChildOccurrences(ctx, tx, now, limit-result.total()); err != nil {
		return fail(err)
	}
	if result.AuthoritiesDeleted, err = pruneChildAuthorities(ctx, tx, now, limit-result.total()); err != nil {
		return fail(err)
	}
	if result.ParentsDeleted, err = pruneChildParents(ctx, tx, now, limit-result.total()); err != nil {
		return fail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return result, nil
}

func pruneExpiredChildTombstones(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	count := 0
	// Delete expired tombstones first so a full tombstone quota can recover.
	ids, err := childPruneIDs(ctx, tx, `SELECT c.child_id FROM child_lineages c JOIN child_parents p USING(gaggle,parent_run)
 WHERE c.tombstoned_ns<=? AND p.settled_ns IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM child_lineages f WHERE f.gaggle=c.gaggle AND f.parent_run=c.parent_run AND f.acknowledged_ns IS NULL)
 ORDER BY c.tombstoned_ns,c.child_id LIMIT ?`, now.Add(-ChildTombstoneRetention).UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM child_lineages WHERE child_id=?`, id); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

func tombstoneChildLineages(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	cutoff := now.Add(-ChildRetention).UnixNano()
	rows, err := tx.QueryContext(ctx, `WITH tombstone_counts AS MATERIALIZED (
 SELECT gaggle,COUNT(*) AS count FROM child_lineages WHERE tombstoned_ns IS NOT NULL GROUP BY gaggle)
 SELECT c.child_id,c.gaggle,COALESCE(h.count,0) FROM child_lineages c JOIN child_parents p USING(gaggle,parent_run)
 LEFT JOIN tombstone_counts h ON h.gaggle=c.gaggle
 WHERE c.tombstoned_ns IS NULL AND c.terminal_ns IS NOT NULL AND c.acknowledged_ns<=? AND p.settled_ns<=?
 AND NOT EXISTS(SELECT 1 FROM child_lineages f WHERE f.gaggle=c.gaggle AND f.parent_run=c.parent_run AND f.acknowledged_ns IS NULL)
 AND NOT EXISTS(SELECT 1 FROM child_parents d WHERE d.gaggle=c.gaggle AND d.parent_run=substr(c.acceptance_id,9))
 AND COALESCE(h.count,0)<?
 AND EXISTS(SELECT 1 FROM triggers t WHERE t.id=c.acceptance_id AND t.state IN ('dispatched','rejected'))
 ORDER BY c.acknowledged_ns,c.child_id LIMIT ?`, cutoff, cutoff, MaxChildTombstones, limit)
	if err != nil {
		return 0, err
	}
	var ids []any
	selected := make(map[string]int)
	for rows.Next() {
		var id, gaggle string
		var existing int
		if err := rows.Scan(&id, &gaggle, &existing); err != nil {
			_ = rows.Close()
			return 0, err
		}
		// Retained idempotency evidence is never evicted to make room. Count
		// every newly selected lineage against its gaggle's remaining quota.
		if existing+selected[gaggle] >= MaxChildTombstones {
			continue
		}
		ids = append(ids, id)
		selected[gaggle]++
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	// The caller caps selection at 100. Two bounded statements preserve the
	// same transaction and release triggers without hundreds of repeated SQL
	// parses/round trips competing with the daemon's maintenance deadline.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	if _, err = tx.ExecContext(ctx, `UPDATE child_lineages SET result_ref='',workspace_ref='',tombstoned_ns=? WHERE child_id IN (`+placeholders+`)`, append([]any{now.UnixNano()}, ids...)...); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM triggers WHERE id IN (SELECT acceptance_id FROM child_lineages WHERE child_id IN (`+placeholders+`))`, ids...); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func pruneChildOccurrences(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	count := 0
	cutoff := now.Add(-ChildRetention).UnixNano()
	// Counters survive as long as any invocation in the occurrence survives.
	rows, err := tx.QueryContext(ctx, `SELECT o.gaggle,o.parent_run,o.occurrence FROM child_occurrences o JOIN child_parents p USING(gaggle,parent_run)
 WHERE p.settled_ns<=? AND NOT EXISTS(SELECT 1 FROM child_lineages c WHERE c.gaggle=o.gaggle AND c.parent_run=o.parent_run AND c.occurrence=o.occurrence)
 ORDER BY o.gaggle,o.parent_run,o.occurrence LIMIT ?`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	var occurrences [][3]string
	for rows.Next() {
		var key [3]string
		if err = rows.Scan(&key[0], &key[1], &key[2]); err != nil {
			_ = rows.Close()
			return 0, err
		}
		occurrences = append(occurrences, key)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	for _, key := range occurrences {
		if _, err = tx.ExecContext(ctx, `DELETE FROM child_occurrences WHERE gaggle=? AND parent_run=? AND occurrence=?`, key[0], key[1], key[2]); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

func pruneChildParents(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	count := 0
	cutoff := now.Add(-ChildRetention).UnixNano()
	rows, err := tx.QueryContext(ctx, `SELECT p.gaggle,p.parent_run FROM child_parents p
 WHERE p.settled_ns<=? AND NOT EXISTS(SELECT 1 FROM child_lineages c WHERE c.gaggle=p.gaggle AND c.parent_run=p.parent_run)
 AND NOT EXISTS(SELECT 1 FROM child_occurrences o WHERE o.gaggle=p.gaggle AND o.parent_run=p.parent_run)
 AND NOT EXISTS(SELECT 1 FROM child_authorities a WHERE a.gaggle=p.gaggle AND a.parent_run=p.parent_run)
 ORDER BY p.gaggle,p.parent_run LIMIT ?`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	var parents [][2]string
	for rows.Next() {
		var key [2]string
		if err = rows.Scan(&key[0], &key[1]); err != nil {
			_ = rows.Close()
			return 0, err
		}
		parents = append(parents, key)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	for _, key := range parents {
		if _, err = tx.ExecContext(ctx, `DELETE FROM child_parents WHERE gaggle=? AND parent_run=?`, key[0], key[1]); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

func childPruneIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
