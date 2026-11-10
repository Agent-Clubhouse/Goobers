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
	ConfirmStart(context.Context, string, string, RunPins) error
	AdoptStart(context.Context, string, string, int64, RunPins) error
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

// ConfirmStart idempotently records a proven current-owner start and its pins.
func (s *Store) ConfirmStart(ctx context.Context, acceptanceID, runID string, pins RunPins) error {
	if pins.AcceptanceID != acceptanceID || pins.RunID != runID {
		return triggerqueue.ErrTransition
	}
	var differs bool
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
   UPDATE control.acceptance SET state='dispatched', start_retry_at=NULL,
     finished_at=COALESCE(finished_at,GREATEST(accepted_at,clock_timestamp()))
   WHERE instance_id=$1 AND id=$2 AND run_id=$3
     AND state IN ('dispatching','dispatched') AND owner_epoch=$4`,
			s.instanceID, acceptanceID, runID, epoch)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		differs, err = s.settleRunPins(txCtx, tx, pins, epoch)
		return err
	})
	if err == nil && differs {
		return ErrRunPinsDiffer
	}
	return err
}

// settleRunPins records a settled start's pins. Different pins for a recorded
// run are permanent: the run is started and its first recorded pins stand.
// They report differs without failing the transaction, so the acceptance
// still settles as dispatched and recovery does not retry a confirmation or
// adoption that can never succeed. Control checks the run against the
// recorded pins and refuses on any mismatch.
func (s *Store) settleRunPins(ctx context.Context, tx pgx.Tx, pins RunPins, epoch int64) (differs bool, err error) {
	err = s.recordRunPins(ctx, tx, pins, epoch)
	if errors.Is(err, ErrRunPinsDiffer) {
		return true, nil
	}
	return false, err
}

// AdoptStart moves a proven predecessor start to the current owner epoch.
func (s *Store) AdoptStart(
	ctx context.Context,
	acceptanceID, runID string,
	predecessorEpoch int64,
	pins RunPins,
) error {
	if predecessorEpoch < 1 || pins.AcceptanceID != acceptanceID || pins.RunID != runID {
		return triggerqueue.ErrTransition
	}
	var differs bool
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.acceptance
			SET owner_epoch=$5, state='dispatched', start_retry_at=NULL,
finished_at=COALESCE(finished_at,GREATEST(accepted_at,clock_timestamp()))
			WHERE instance_id=$1 AND id=$2 AND run_id=$3
			  AND state IN ('dispatching','dispatched') AND owner_epoch=$4
			  AND owner_epoch<$5`,
			s.instanceID, acceptanceID, runID, predecessorEpoch, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		differs, err = s.settleRunPins(txCtx, tx, pins, epoch)
		return err
	})
	if err == nil && differs {
		return ErrRunPinsDiffer
	}
	return err
}

var _ StartAdmissionStore = (*Store)(nil)
