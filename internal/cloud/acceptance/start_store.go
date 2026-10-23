package acceptance

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// StartAuthority is the durable identity authorized to start one accepted run.
type StartAuthority struct {
	AcceptanceID string
	RunID        string
	OwnerEpoch   int64
}

// StartAdmissionStore is the fenced seam used by the cloud engine starter.
type StartAdmissionStore interface {
	ReadStartAuthority(context.Context, string) (StartAuthority, error)
	ConfirmStart(context.Context, string, string) error
	AdoptStart(context.Context, string, string, int64) error
}

// ReadStartAuthority loads identity while holding the current lease fence.
func (s *Store) ReadStartAuthority(ctx context.Context, acceptanceID string) (StartAuthority, error) {
	var authority StartAuthority
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		err := tx.QueryRow(txCtx, `
			SELECT id, run_id, owner_epoch
			FROM control.acceptance
			WHERE instance_id=$1 AND id=$2 AND state IN ('dispatching','dispatched')
			  AND owner_epoch>0 AND owner_epoch<=$3
			FOR SHARE`,
			s.instanceID, acceptanceID, epoch,
		).Scan(&authority.AcceptanceID, &authority.RunID, &authority.OwnerEpoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return triggerqueue.ErrTransition
		}
		return err
	})
	return authority, err
}

// ConfirmStart idempotently records a proven current-owner start.
func (s *Store) ConfirmStart(ctx context.Context, acceptanceID, runID string) error {
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
   UPDATE control.acceptance SET state='dispatched', start_retry_at=NULL,
     finished_at=COALESCE(finished_at,GREATEST(accepted_at,clock_timestamp()))
   WHERE instance_id=$1 AND id=$2 AND run_id=$3
     AND state IN ('dispatching','dispatched') AND owner_epoch=$4`,
			s.instanceID, acceptanceID, runID, epoch)
		return changed(tag.RowsAffected(), err)
	})
}

// AdoptStart moves a proven predecessor start to the current owner epoch.
func (s *Store) AdoptStart(
	ctx context.Context,
	acceptanceID, runID string,
	predecessorEpoch int64,
) error {
	if predecessorEpoch < 1 {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.acceptance
			SET owner_epoch=$5, state='dispatched', start_retry_at=NULL,
finished_at=COALESCE(finished_at,GREATEST(accepted_at,clock_timestamp()))
			WHERE instance_id=$1 AND id=$2 AND run_id=$3
			  AND state IN ('dispatching','dispatched') AND owner_epoch=$4
			  AND owner_epoch<$5`,
			s.instanceID, acceptanceID, runID, predecessorEpoch, epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}

var _ StartAdmissionStore = (*Store)(nil)
