package triggerqueue

import (
	"context"
	"errors"
)

// WithChildLaunch serializes the last parent cancellation check and publication
// of the reserved child's immutable journal with the parent fence transaction.
// publish must be bounded and must NOT call Store methods or start stage effects.
// Its success means only that an exact journal is durable; the caller releases
// its execution barrier only after this transaction commits. A publish error is
// ambiguous and never licenses replay without journal reconciliation.
func (s *Store) WithChildLaunch(ctx context.Context, identity ChildIdentity, publish func() error) error {
	if !identity.valid() || publish == nil {
		return errors.New("triggerqueue: invalid child launch barrier")
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
	if child.State != ChildQueued || child.CancellationRequested || !child.TombstonedAt.IsZero() {
		return ErrTransition
	}
	var state State
	if err = tx.QueryRowContext(ctx, `SELECT state FROM triggers WHERE id=?`, child.AcceptanceID).Scan(&state); err != nil {
		return err
	}
	if state != Dispatching {
		return ErrTransition
	}
	if err = publish(); err != nil {
		return err
	}
	return tx.Commit()
}
