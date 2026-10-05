package triggerqueue

import (
	"context"
	"errors"
	"time"
)

// RequeueChild resolves a proven absent/deferred handoff without losing a parent
// cancellation that raced its claim. Ordinary source-neutral Requeue is unchanged.
func (s *Store) RequeueChild(ctx context.Context, identity ChildIdentity, reason string, now time.Time) error {
	if !identity.valid() || len(reason) > 1024 || now.IsZero() {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(identity)...))
	if err != nil {
		return err
	}
	state := Accepted
	var finished any
	if err = childParentOpen(ctx, tx, identity.ChildParent); errors.Is(err, ErrParentCancelled) || errors.Is(err, ErrParentSettled) {
		state = Rejected
		reason = "parent cancellation requested"
		finished = now.UnixNano()
	} else if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE triggers SET state=?,run_id='',reason=?,finished_ns=? WHERE id=? AND state='dispatching' AND finished_ns IS NULL`, state, reason, finished, c.AcceptanceID)
	if err = changed(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

// ChildRejection returns the durable rejection time; it never synthesizes an
// execution result or interprets an uncertain dispatch as a rejected start.
func (s *Store) ChildRejection(ctx context.Context, identity ChildIdentity) (time.Time, string, error) {
	var at int64
	var reason string
	err := s.db.QueryRowContext(ctx, `SELECT t.finished_ns,t.reason FROM triggers t JOIN child_lineages c ON c.acceptance_id=t.id WHERE c.gaggle=? AND c.parent_run=? AND c.occurrence=? AND c.invocation_key=? AND t.state='rejected' AND t.finished_ns IS NOT NULL`, childArgs(identity)...).Scan(&at, &reason)
	return time.Unix(0, at).UTC(), reason, err
}

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
	if child.ExecutionEpoch != 0 || child.State != ChildQueued || child.CancellationRequested || !child.TombstonedAt.IsZero() {
		return ErrTransition
	}
	var state State
	if err = tx.QueryRowContext(ctx, `SELECT state FROM triggers WHERE id=?`, child.AcceptanceID).Scan(&state); err != nil {
		return err
	}
	if state != Dispatching {
		return ErrTransition
	}
	if err = initialChildStartOpen(ctx, tx, child); err != nil {
		return err
	}
	if err = publish(); err != nil {
		return err
	}
	return tx.Commit()
}
