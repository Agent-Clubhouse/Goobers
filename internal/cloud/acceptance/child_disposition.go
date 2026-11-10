package acceptance

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func validDispositionAction(action string) bool {
	return action == "merge" || action == "replace" || action == "discard"
}

// RequestChildDisposition stores one exact terminal parent choice.
func (s *Store) RequestChildDisposition(
	ctx context.Context,
	request triggerqueue.ChildDispositionRequest,
	now time.Time,
) (result triggerqueue.ChildDisposition, err error) {
	if !childIdentityValid(request.Identity) ||
		!validDispositionAction(request.Action) ||
		!blobstore.ValidDigest(request.ResultRef) ||
		request.Authority.ChildParent != request.Identity.ChildParent ||
		request.Authority.StageOccurrence != request.Identity.StageOccurrence ||
		(request.ExpectedRequestDigest != "" &&
			!blobstore.ValidDigest(request.ExpectedRequestDigest)) ||
		now.IsZero() {
		return result, triggerqueue.ErrTransition
	}
	err = s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, request.Identity, epoch)
		if err != nil {
			return err
		}
		if err := s.checkChildAuthorityTx(txCtx, tx, request.Authority); err != nil {
			return err
		}
		if child.CancellationRequested || !child.TombstonedAt.IsZero() ||
			!child.State.Terminal() || child.ResultRef != request.ResultRef {
			return triggerqueue.ErrTransition
		}
		current, readErr := s.childDisposition(txCtx, tx, child)
		if readErr == nil {
			result, err = s.reviseChildDisposition(txCtx, tx, child, current, request, epoch)
			return err
		}
		if !errors.Is(readErr, triggerqueue.ErrChildDispositionPending) {
			return readErr
		}
		if request.ExpectedRequestDigest != "" || !child.AcknowledgedAt.IsZero() {
			return triggerqueue.ErrTransition
		}
		var requestedAt time.Time
		if err := tx.QueryRow(txCtx, `
			INSERT INTO control.child_disposition (
				instance_id,child_id,request_digest,action,result_ref,
				attempt_id,requested_at,owner_epoch
			) VALUES ($1,$2,'',$3,$4,$5,clock_timestamp(),$6)
			RETURNING requested_at`,
			s.instanceID, child.ChildID, request.Action,
			request.ResultRef, request.Authority.AttemptID, epoch,
		).Scan(&requestedAt); err != nil {
			return err
		}
		result = triggerqueue.ChildDisposition{
			Identity: request.Identity, Action: request.Action,
			ResultRef: request.ResultRef, AttemptID: request.Authority.AttemptID,
			RequestedAt: requestedAt.UTC(),
		}
		digest := result.RequestDigest()
		if _, err := tx.Exec(txCtx, `
			UPDATE control.child_disposition SET request_digest=$3
			WHERE instance_id=$1 AND child_id=$2 AND owner_epoch=$4`,
			s.instanceID, child.ChildID, digest, epoch,
		); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

// ChildDisposition reads one retained choice and its optional plan.
func (s *Store) ChildDisposition(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
) (triggerqueue.ChildDisposition, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil || !child.TombstonedAt.IsZero() {
		return triggerqueue.ChildDisposition{}, triggerqueue.ErrTransition
	}
	return s.childDisposition(ctx, s.pool, child)
}

func (s *Store) childDisposition(
	ctx context.Context,
	query rowQuerier,
	child triggerqueue.ChildRecord,
) (triggerqueue.ChildDisposition, error) {
	result := triggerqueue.ChildDisposition{Identity: child.Identity}
	var appliedAt *time.Time
	err := query.QueryRow(ctx, `
		SELECT action,result_ref,attempt_id,requested_at,
		       plan_digest,applied_at,previous_digest
		FROM control.child_disposition
		WHERE instance_id=$1 AND child_id=$2`,
		s.instanceID, child.ChildID,
	).Scan(
		&result.Action, &result.ResultRef, &result.AttemptID, &result.RequestedAt,
		&result.PlanDigest, &appliedAt, &result.PreviousDigest,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, triggerqueue.ErrChildDispositionPending
	}
	if err != nil {
		return result, err
	}
	result.RequestedAt = result.RequestedAt.UTC()
	if appliedAt != nil {
		result.AppliedAt = appliedAt.UTC()
	}
	if result.PlanDigest != "" {
		if s.childBlobs == nil {
			return triggerqueue.ChildDisposition{}, triggerqueue.ErrConflict
		}
		result.Plan, err = s.childBlobs.Get(ctx, result.PlanDigest)
		if err != nil || !verifyDigestData(result.PlanDigest, result.Plan) {
			return triggerqueue.ChildDisposition{}, triggerqueue.ErrConflict
		}
	}
	if !validDispositionAction(result.Action) ||
		result.ResultRef != child.ResultRef ||
		result.RequestDigest() == "" {
		return triggerqueue.ChildDisposition{}, triggerqueue.ErrConflict
	}
	return result, nil
}

// KeepChildDispositionPlan stores immutable plan bytes before effects.
func (s *Store) KeepChildDispositionPlan(
	ctx context.Context,
	expected triggerqueue.ChildDisposition,
	plan []byte,
) error {
	if s.childBlobs == nil || len(plan) == 0 ||
		len(plan) > triggerqueue.MaxChildSnapshotReceiptBytes {
		return triggerqueue.ErrTransition
	}
	digest := childDigest(plan)
	if err := s.childBlobs.Put(ctx, digest, plan); err != nil {
		return err
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, expected.Identity, epoch)
		if err != nil {
			return err
		}
		if child.CancellationRequested || !child.TombstonedAt.IsZero() {
			return triggerqueue.ErrParentCancelled
		}
		current, err := s.childDisposition(txCtx, tx, child)
		if err != nil {
			return err
		}
		if current.RequestDigest() != expected.RequestDigest() {
			return triggerqueue.ErrConflict
		}
		if len(current.Plan) != 0 {
			if !bytes.Equal(current.Plan, plan) {
				return triggerqueue.ErrConflict
			}
			return nil
		}
		if !current.AppliedAt.IsZero() {
			return triggerqueue.ErrTransition
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_disposition
			SET plan_digest=$3,owner_epoch=$4
			WHERE instance_id=$1 AND child_id=$2
			  AND plan_digest='' AND owner_epoch=$4`,
			s.instanceID, child.ChildID, digest, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		return s.insertChildObjectPointers(txCtx, tx, child.ChildID, epoch,
			childObject{digest, "disposition-plan", len(plan)})
	})
}

// CompleteChildDisposition records verified application and releases the slot.
func (s *Store) CompleteChildDisposition(
	ctx context.Context,
	expected triggerqueue.ChildDisposition,
	now time.Time,
) error {
	if !blobstore.ValidDigest(expected.PlanDigest) || now.IsZero() {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, expected.Identity, epoch)
		if err != nil {
			return err
		}
		current, err := s.childDisposition(txCtx, tx, child)
		if err != nil {
			return err
		}
		if current.RequestDigest() != expected.RequestDigest() ||
			current.PlanDigest != expected.PlanDigest ||
			!child.TombstonedAt.IsZero() || !child.State.Terminal() {
			return triggerqueue.ErrConflict
		}
		if !current.AppliedAt.IsZero() {
			return nil
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_disposition
			SET applied_at=clock_timestamp(),owner_epoch=$3
			WHERE instance_id=$1 AND child_id=$2
			  AND applied_at IS NULL AND owner_epoch=$3`,
			s.instanceID, child.ChildID, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		tag, err = tx.Exec(txCtx, `
			UPDATE control.child_lineage
			SET acknowledged_at=clock_timestamp(),updated_at=clock_timestamp(),
			    owner_epoch=$3
			WHERE instance_id=$1 AND child_id=$2
			  AND acknowledged_at IS NULL AND owner_epoch=$3`,
			s.instanceID, child.ChildID, epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}
