package triggerqueue

import (
	"context"
	"errors"
)

// WithChildResume serializes recovery ownership with the parent cancellation
// fence. ready must only establish an idle, cancellable runner; it must not
// call Store methods or allow stage effects until this transaction commits.
// Initial publication continues to use the stricter WithChildLaunch barrier.
func (s *Store) WithChildResume(ctx context.Context, identity ChildIdentity, ready func() error) error {
	if !identity.valid() || ready == nil {
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
	if (child.State != ChildQueued && child.State != ChildRunning) || child.CancellationRequested || !child.TombstonedAt.IsZero() {
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
