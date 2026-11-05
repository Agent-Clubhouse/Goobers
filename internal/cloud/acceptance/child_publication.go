package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func validPublicationAction(action string) bool { return action == "branch" || action == "pr" }

// PrepareChildPublication retains one immutable publication intent.
func (s *Store) PrepareChildPublication(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	action string,
	intent []byte,
) (triggerqueue.ChildPublication, error) {
	return s.prepareChildPublication(ctx, identity, "", action, intent)
}

// PrepareChildExecutionPublication additionally binds the accepted execution.
func (s *Store) PrepareChildExecutionPublication(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	runID string,
	action string,
	intent []byte,
) (triggerqueue.ChildPublication, error) {
	if !validText(runID, 256, true) {
		return triggerqueue.ChildPublication{}, triggerqueue.ErrTransition
	}
	return s.prepareChildPublication(ctx, identity, runID, action, intent)
}

func (s *Store) prepareChildPublication(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	runID string,
	action string,
	intent []byte,
) (publication triggerqueue.ChildPublication, err error) {
	if !childIdentityValid(identity) || !validPublicationAction(action) ||
		s.childBlobs == nil || len(intent) == 0 ||
		len(intent) > triggerqueue.MaxChildPublicationIntentBytes ||
		!json.Valid(intent) {
		return publication, triggerqueue.ErrChildPublicationUnavailable
	}
	digest := childDigest(intent)
	if err := s.childBlobs.Put(ctx, digest, intent); err != nil {
		return publication, err
	}
	err = s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, identity, epoch)
		if err != nil || !child.TombstonedAt.IsZero() {
			return triggerqueue.ErrChildPublicationUnavailable
		}
		if runID == "" {
			runID = child.RunID
		}
		var existingDigest, existingRun, state, receiptDigest string
		var createdAt, updatedAt time.Time
		err = tx.QueryRow(txCtx, `
			SELECT intent_digest,execution_run_id,state,receipt_digest,created_at,updated_at
			FROM control.child_publication
			WHERE instance_id=$1 AND child_id=$2 AND action=$3
			FOR UPDATE`,
			s.instanceID, child.ChildID, action,
		).Scan(
			&existingDigest, &existingRun, &state, &receiptDigest,
			&createdAt, &updatedAt,
		)
		if err == nil {
			if existingDigest != digest || existingRun != runID {
				return triggerqueue.ErrConflict
			}
			publication = triggerqueue.ChildPublication{
				ExecutionRunID: runID, Identity: identity, Action: action,
				Intent: bytes.Clone(intent), Digest: digest, State: state,
				ReceiptDigest: receiptDigest,
				CreatedAt:     createdAt.UTC(), UpdatedAt: updatedAt.UTC(),
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if child.RunID != runID || child.State.Terminal() ||
			child.CancellationRequested || !child.AcknowledgedAt.IsZero() {
			return triggerqueue.ErrTransition
		}
		err = tx.QueryRow(txCtx, `
			INSERT INTO control.child_publication (
				instance_id,child_id,action,execution_run_id,intent_digest,
				state,owner_epoch
			) VALUES ($1,$2,$3,$4,$5,'prepared',$6)
			RETURNING created_at,updated_at`,
			s.instanceID, child.ChildID, action, runID, digest, epoch,
		).Scan(&createdAt, &updatedAt)
		if err != nil {
			return err
		}
		if err := s.insertChildObjectPointers(txCtx, tx, child.ChildID, epoch,
			childObject{digest, "publication-intent", len(intent)}); err != nil {
			return err
		}
		publication = triggerqueue.ChildPublication{
			ExecutionRunID: runID, Identity: identity, Action: action,
			Intent: bytes.Clone(intent), Digest: digest, State: "prepared",
			CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(),
		}
		return nil
	})
	return publication, err
}

// ChildPublication reads verified intent and optional confirmed receipt bytes.
func (s *Store) ChildPublication(
	ctx context.Context,
	identity triggerqueue.ChildIdentity,
	action string,
) (triggerqueue.ChildPublication, error) {
	if !childIdentityValid(identity) || !validPublicationAction(action) || s.childBlobs == nil {
		return triggerqueue.ChildPublication{}, triggerqueue.ErrChildPublicationUnavailable
	}
	child, err := s.GetChild(ctx, identity)
	if err != nil || !child.TombstonedAt.IsZero() {
		return triggerqueue.ChildPublication{}, triggerqueue.ErrChildPublicationUnavailable
	}
	out := triggerqueue.ChildPublication{Identity: identity, Action: action}
	err = s.pool.QueryRow(ctx, `
		SELECT execution_run_id,intent_digest,state,receipt_digest,created_at,updated_at
		FROM control.child_publication
		WHERE instance_id=$1 AND child_id=$2 AND action=$3`,
		s.instanceID, child.ChildID, action,
	).Scan(
		&out.ExecutionRunID, &out.Digest, &out.State, &out.ReceiptDigest,
		&out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return triggerqueue.ChildPublication{}, triggerqueue.ErrChildPublicationPending
	}
	if err != nil {
		return out, err
	}
	out.Intent, err = s.childBlobs.Get(ctx, out.Digest)
	if err != nil || !verifyDigestData(out.Digest, out.Intent) || !json.Valid(out.Intent) {
		return triggerqueue.ChildPublication{}, triggerqueue.ErrChildPublicationUnavailable
	}
	if out.ReceiptDigest != "" {
		out.Receipt, err = s.childBlobs.Get(ctx, out.ReceiptDigest)
		if err != nil || !verifyDigestData(out.ReceiptDigest, out.Receipt) ||
			!json.Valid(out.Receipt) {
			return triggerqueue.ChildPublication{}, triggerqueue.ErrChildPublicationUnavailable
		}
	}
	out.CreatedAt, out.UpdatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC()
	return out, nil
}

// BeginChildPublicationEffect admits one prepared effect.
func (s *Store) BeginChildPublicationEffect(
	ctx context.Context,
	expected triggerqueue.ChildPublication,
) error {
	return s.beginChildPublication(ctx, expected, "")
}

// BeginChildExecutionPublicationEffect repeats the execution fence.
func (s *Store) BeginChildExecutionPublicationEffect(
	ctx context.Context,
	expected triggerqueue.ChildPublication,
	runID string,
) error {
	if !validText(runID, 256, true) {
		return triggerqueue.ErrTransition
	}
	return s.beginChildPublication(ctx, expected, runID)
}

func (s *Store) beginChildPublication(
	ctx context.Context,
	expected triggerqueue.ChildPublication,
	runID string,
) error {
	if !childIdentityValid(expected.Identity) || !validPublicationAction(expected.Action) {
		return triggerqueue.ErrChildPublicationUnavailable
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, expected.Identity, epoch)
		if err != nil {
			return err
		}
		if runID == "" {
			runID = child.RunID
		}
		if runID != child.RunID || runID != expected.ExecutionRunID ||
			child.State.Terminal() || child.CancellationRequested ||
			!child.AcknowledgedAt.IsZero() {
			return triggerqueue.ErrTransition
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_publication
			SET state='effect_pending',updated_at=clock_timestamp(),owner_epoch=$6
			WHERE instance_id=$1 AND child_id=$2 AND action=$3
			  AND execution_run_id=$4 AND intent_digest=$5
			  AND state=$7 AND owner_epoch=$6`,
			s.instanceID, child.ChildID, expected.Action,
			runID, expected.Digest, epoch, expected.State,
		)
		if expected.State == "effect_pending" && tag.RowsAffected() == 1 {
			return nil
		}
		if expected.State != "prepared" {
			return triggerqueue.ErrTransition
		}
		return changed(tag.RowsAffected(), err)
	})
}

// ConfirmChildPublication stores exact observed effect evidence.
func (s *Store) ConfirmChildPublication(
	ctx context.Context,
	expected triggerqueue.ChildPublication,
	receipt []byte,
) error {
	if !childIdentityValid(expected.Identity) || !validPublicationAction(expected.Action) ||
		s.childBlobs == nil || len(receipt) == 0 ||
		len(receipt) > triggerqueue.MaxChildPublicationReceiptBytes ||
		!json.Valid(receipt) {
		return triggerqueue.ErrChildPublicationUnavailable
	}
	digest := childDigest(receipt)
	if err := s.childBlobs.Put(ctx, digest, receipt); err != nil {
		return err
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		child, err := s.lockChild(txCtx, tx, expected.Identity, epoch)
		if err != nil {
			return err
		}
		var state, currentDigest, receiptDigest string
		if err := tx.QueryRow(txCtx, `
			SELECT state,intent_digest,receipt_digest
			FROM control.child_publication
			WHERE instance_id=$1 AND child_id=$2 AND action=$3
			FOR UPDATE`,
			s.instanceID, child.ChildID, expected.Action,
		).Scan(&state, &currentDigest, &receiptDigest); err != nil {
			return err
		}
		if currentDigest != expected.Digest {
			return triggerqueue.ErrConflict
		}
		if state == "confirmed" {
			if receiptDigest == digest {
				return nil
			}
			return triggerqueue.ErrConflict
		}
		if state != "effect_pending" {
			return triggerqueue.ErrTransition
		}
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_publication
			SET state='confirmed',receipt_digest=$4,
			    updated_at=clock_timestamp(),owner_epoch=$5
			WHERE instance_id=$1 AND child_id=$2 AND action=$3
			  AND state='effect_pending' AND owner_epoch=$5`,
			s.instanceID, child.ChildID, expected.Action, digest, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		return s.insertChildObjectPointers(txCtx, tx, child.ChildID, epoch,
			childObject{digest, "publication-receipt", len(receipt)})
	})
}
