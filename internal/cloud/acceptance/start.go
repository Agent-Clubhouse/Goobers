package acceptance

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ReadStart fences and claims durable start identity for this controller.
func (s *Store) ReadStart(ctx context.Context, acceptanceID string) (apicontract.StartIdentity, error) {
	var identity apicontract.StartIdentity
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		var state triggerqueue.State
		var owner *int64
		if err := tx.QueryRow(txCtx, `
			SELECT id, run_id, state, owner_epoch
			FROM control.acceptance
			WHERE instance_id=$1 AND id=$2
			FOR UPDATE`,
			s.instanceID, acceptanceID,
		).Scan(&identity.AcceptanceID, &identity.RunID, &state, &owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return triggerqueue.ErrTransition
			}
			return err
		}
		switch {
		case identity.RunID == "", state == triggerqueue.Rejected, state == triggerqueue.Dispatched:
			return triggerqueue.ErrTransition
		case state == triggerqueue.Accepted:
			tag, err := tx.Exec(txCtx, `
				UPDATE control.acceptance
				SET state='dispatching', owner_epoch=$3
				WHERE instance_id=$1 AND id=$2 AND state='accepted'`,
				s.instanceID, acceptanceID, epoch,
			)
			if err := changed(tag.RowsAffected(), err); err != nil {
				return err
			}
		case state == triggerqueue.Dispatching && owner != nil && *owner <= epoch:
		default:
			return triggerqueue.ErrTransition
		}
		identity.InstanceID = s.instanceID
		identity.StarterID = s.starterID
		identity.OwnerEpoch = epoch
		return nil
	})
	return identity, err
}

// ConfirmStart fences mine/adopt completion against durable ownership.
func (s *Store) ConfirmStart(ctx context.Context, identity apicontract.StartIdentity, adopt bool) error {
	if identity.AcceptanceID == "" || identity.RunID == "" ||
		identity.InstanceID != s.instanceID || identity.StarterID != s.starterID {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		if identity.OwnerEpoch != epoch {
			return triggerqueue.ErrTransition
		}
		if adopt {
			tag, err := tx.Exec(txCtx, `
				UPDATE control.acceptance
				SET owner_epoch=$4
				WHERE instance_id=$1 AND id=$2 AND run_id=$3
				  AND state='dispatching' AND owner_epoch<$4`,
				s.instanceID, identity.AcceptanceID, identity.RunID, epoch,
			)
			return changed(tag.RowsAffected(), err)
		}
		var exists bool
		if err := tx.QueryRow(txCtx, `
			SELECT EXISTS (
				SELECT 1 FROM control.acceptance
				WHERE instance_id=$1 AND id=$2 AND run_id=$3
				  AND state='dispatching' AND owner_epoch=$4
			)`,
			s.instanceID, identity.AcceptanceID, identity.RunID, epoch,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return triggerqueue.ErrTransition
		}
		return nil
	})
}

// CheckStartLease performs a read-only current-lease fence check.
func (s *Store) CheckStartLease(ctx context.Context) error {
	return s.withFence(ctx, func(context.Context, pgx.Tx, int64) error { return nil })
}
