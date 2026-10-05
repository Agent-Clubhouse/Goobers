package triggerqueue

import "context"

// ChildJournalOwned protects exact family journals while full queue custody is
// retained, including source epochs and late publication evidence. Tombstoning
// is the existing bounded family release point; this query loads no plan/source
// bytes and never authorizes execution or grants access to journal contents.
func (s *Store) ChildJournalOwned(ctx context.Context, gaggle, runID string) (bool, error) {
	if !validChildText(gaggle, 128, true) || !validChildText(runID, 256, true) {
		return false, nil
	}
	var owned bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM child_lineages c WHERE c.gaggle=? AND c.tombstoned_ns IS NULL AND (c.parent_run=? OR c.acceptance_id=? OR EXISTS(SELECT 1 FROM child_execution_epochs e WHERE e.child_id=c.child_id AND e.run_id=?)))`, gaggle, runID, "trigger-"+runID, runID).Scan(&owned)
	return owned, err
}
