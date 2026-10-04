package triggerqueue

import (
	"context"
	"database/sql"
)

// ChildForRun resolves host custody from an already authenticated run identity.
// This internal lookup is not a portal listing or an authorization decision.
// Tombstones are returned so an expired child cannot become an ordinary run.
func (s *Store) ChildForRun(ctx context.Context, runID string) (ChildRecord, error) {
	if !validChildText(runID, 256, true) {
		return ChildRecord{}, sql.ErrNoRows
	}
	child, err := scanChild(s.db.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+" WHERE c.acceptance_id=?", "trigger-"+runID))
	return child, err
}
