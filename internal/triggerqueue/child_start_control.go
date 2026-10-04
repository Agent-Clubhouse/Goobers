package triggerqueue

import (
	"context"
	"database/sql"
)

// Only the original execution belongs to this queue acceptance. Human epochs
// have their own custody and must never inherit its cancellation request.
func initialChildStartOpen(ctx context.Context, tx *sql.Tx, child ChildRecord) error {
	if child.ExecutionEpoch != 0 {
		return nil
	}
	var cancelled bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM start_controls WHERE acceptance_id=? AND cancel_ns IS NOT NULL)`, child.AcceptanceID).Scan(&cancelled); err != nil {
		return err
	}
	if cancelled {
		return ErrTransition
	}
	return nil
}

// ChildInitialStartCancelled is an early recovery refusal; publication/resume
// still enforce the same fence transactionally before releasing any effects.
func (s *Store) ChildInitialStartCancelled(ctx context.Context, child ChildRecord) (bool, error) {
	if child.ExecutionEpoch != 0 {
		return false, nil
	}
	var cancelled bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM start_controls WHERE acceptance_id=? AND cancel_ns IS NOT NULL)`, child.AcceptanceID).Scan(&cancelled)
	return cancelled, err
}
