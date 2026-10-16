package triggerqueue

import (
	"context"
	"errors"
)

// WithChildResume establishes idle recovery ownership for the exact accepted
// execution while holding the same parent cancellation fence as WithChildLaunch.
// It does not authorize a new run. ready must be bounded, must not call Store
// methods, and must not release stage effects until this transaction commits.
func (s *Store) WithChildResume(ctx context.Context, identity ChildIdentity, runID string, ready func() error) error {
	if !identity.valid() || !validChildText(runID, 256, true) || ready == nil {
		return errors.New("triggerqueue: invalid child resume barrier")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = childParentOpen(ctx, tx, identity.ChildParent); err != nil {
		return err
	}
	child, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(identity)...))
	if err != nil {
		return err
	}
	if child.RunID != runID || (child.State != ChildQueued && child.State != ChildRunning) || child.CancellationRequested || !child.TombstonedAt.IsZero() {
		return ErrTransition
	}
	var state State
	if err = tx.QueryRowContext(ctx, `SELECT state FROM triggers WHERE id=?`, child.AcceptanceID).Scan(&state); err != nil {
		return err
	}
	if state != Dispatching && state != Dispatched {
		return ErrTransition
	}
	if err = ready(); err != nil {
		return err
	}
	return tx.Commit()
}
