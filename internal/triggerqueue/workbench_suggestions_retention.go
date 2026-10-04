package triggerqueue

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// PruneWorkbenchSuggestions bounds review maintenance to limit row transitions.
// An unlinked acceptance or linked uncertain proposal never ages out. The review
// pins its proposal until the review itself compacts; no provider I/O occurs here.
func (s *Store) PruneWorkbenchSuggestions(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return 0, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := now.Add(-WorkbenchCommandRetention).UnixNano()
	ids, err := childPruneIDs(ctx, tx, `SELECT id FROM workbench_suggestions WHERE state='tombstoned' AND tombstoned_ns<=? ORDER BY id LIMIT ?`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM workbench_suggestions WHERE id=? AND state='tombstoned'`, id); err != nil {
			return 0, err
		}
	}
	count := len(ids)
	ids, err = childPruneIDs(ctx, tx, `SELECT s.id FROM workbench_suggestions s WHERE s.tombstoned_ns IS NULL AND
 ((s.state='rejected' AND s.completed_ns<=?) OR
 (s.state='linked' AND s.linked_ns<=? AND EXISTS(SELECT 1 FROM workbench_proposals p WHERE p.id=s.proposal_id AND p.state IN ('confirmed','observed','not-applied') AND p.completed_ns<=?)))
 ORDER BY s.id LIMIT ?`, cutoff, cutoff, cutoff, limit-count)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE workbench_suggestions SET state='tombstoned',input='',reserved_bytes=0,completed_ns=COALESCE(completed_ns,?),tombstoned_ns=? WHERE id=?`, now.UnixNano(), now.UnixNano(), id); err != nil {
			return 0, err
		}
	}
	return count + len(ids), tx.Commit()
}

// SuggestionRunRetained protects the complete producer journal, including the
// exact artifact bytes, while any full review still references it.
func (s *Store) SuggestionRunRetained(ctx context.Context, gaggle, runID string) (bool, error) {
	if !apiv1.ValidRunID(runID) || !validChildText(gaggle, 128, true) {
		return false, ErrTransition
	}
	var retained bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workbench_suggestions WHERE gaggle=? AND origin_run=? AND tombstoned_ns IS NULL)`, gaggle, runID).Scan(&retained)
	return retained, err
}

// RetainedSuggestionGenerations bounds the global generation pin inventory.
// Exceeding this defensive metadata bound refuses pruning rather than dropping
// dependencies. Payloads are never loaded for retention or monitoring.
func (s *Store) RetainedSuggestionGenerations(ctx context.Context) ([]string, error) {
	const maxPins = 10000
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT config_generation FROM workbench_suggestions WHERE tombstoned_ns IS NULL ORDER BY config_generation LIMIT ?`, maxPins+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []string{}
	for rows.Next() {
		var pin string
		if err = rows.Scan(&pin); err != nil {
			return nil, err
		}
		if len(result) == maxPins || !validWorkbenchDigest(pin) {
			return nil, ErrTransition
		}
		result = append(result, pin)
	}
	return result, rows.Err()
}
