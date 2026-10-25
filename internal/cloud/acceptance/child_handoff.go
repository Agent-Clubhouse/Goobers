package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrChildObservationChanged asks the observer to rebuild a snapshot. No grant
// is revoked and no handoff is sealed from the changed snapshot.
var ErrChildObservationChanged = errors.New("cloud acceptance: child observation changed")

// ConfirmChildHandoff linearizes the validated control-owned snapshot with
// revocation and, when the worker proved its writers stopped, durable recovery.
// Immutable payload/source validation happens before this bounded transaction.
func (s *Store) ConfirmChildHandoff(ctx context.Context, grant triggerqueue.ChildAuthority, expected *triggerqueue.ChildRecord, receipt []byte, finish, stopped bool) error {
	if !childAuthorityValid(grant) || len(receipt) > 16384 || (stopped && !finish) {
		return triggerqueue.ErrChildAuthorityChanged
	}
	return s.withFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		retained, live, err := s.lockHandoffAuthority(ctx, tx, epoch, grant)
		if err != nil {
			return err
		}
		if len(retained) != 0 {
			if sameHandoffReceipt(retained, receipt) {
				return nil
			}
			return triggerqueue.ErrChildAuthorityChanged
		}
		if !live {
			return triggerqueue.ErrChildAuthorityChanged
		}
		if err := s.confirmHandoffChild(ctx, tx, epoch, grant, expected); err != nil {
			return err
		}
		if !finish {
			return nil
		}
		var sealed []byte
		if stopped && len(receipt) != 0 {
			sealed = receipt
		}
		_, err = tx.Exec(ctx, `UPDATE control.child_authority
   SET revoked=true,stopped_handoff=$6
   WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND occurrence=$4 AND owner_epoch=$5`,
			s.instanceID, grant.Gaggle, grant.ParentRunID, grant.StageOccurrence, epoch, sealed)
		return err
	})
}

// RecoverChildHandoff reads only a previously sealed stopped receipt, under
// the same controller, family and grant fence. Expiry cannot erase stop proof.
func (s *Store) RecoverChildHandoff(ctx context.Context, grant triggerqueue.ChildAuthority) ([]byte, error) {
	if !childAuthorityValid(grant) {
		return nil, triggerqueue.ErrChildAuthorityChanged
	}
	var receipt []byte
	err := s.withFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		var err error
		receipt, _, err = s.lockHandoffAuthority(ctx, tx, epoch, grant)
		return err
	})
	return receipt, err
}

func (s *Store) lockHandoffAuthority(ctx context.Context, tx pgx.Tx, epoch int64, grant triggerqueue.ChildAuthority) ([]byte, bool, error) {
	var parentEpoch int64
	var cancelled, settled *time.Time
	err := tx.QueryRow(ctx, `SELECT owner_epoch,cancelled_at,settled_at FROM control.child_parent
  WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 FOR UPDATE`,
		s.instanceID, grant.Gaggle, grant.ParentRunID).Scan(&parentEpoch, &cancelled, &settled)
	if err != nil {
		return nil, false, err
	}
	if parentEpoch != epoch {
		return nil, false, ErrStaleChildEpoch
	}
	if cancelled != nil {
		return nil, false, triggerqueue.ErrParentCancelled
	}
	if settled != nil {
		return nil, false, triggerqueue.ErrParentSettled
	}
	var valid, live bool
	var receipt []byte
	err = tx.QueryRow(ctx, `SELECT grant_id=$5 AND attempt_id=$6 AND config_digest=$7 AND policy_digest=$8 AND owner_epoch=$9,
  NOT revoked AND expires_at>clock_timestamp(),stopped_handoff
  FROM control.child_authority WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND occurrence=$4 FOR UPDATE`,
		s.instanceID, grant.Gaggle, grant.ParentRunID, grant.StageOccurrence, grant.GrantID, grant.AttemptID, grant.ConfigDigest, grant.PolicyDigest, epoch).Scan(&valid, &live, &receipt)
	if err != nil {
		return nil, false, err
	}
	if !valid {
		return nil, false, triggerqueue.ErrChildAuthorityChanged
	}
	return receipt, live, nil
}

func (s *Store) confirmHandoffChild(ctx context.Context, tx pgx.Tx, epoch int64, grant triggerqueue.ChildAuthority, expected *triggerqueue.ChildRecord) error {
	// The family row is locked first, just as admission, cancellation and child
	// transitions lock it. A new start cannot cross this read and revocation.
	rows, err := tx.Query(ctx, childColumns+` WHERE c.instance_id=$1 AND c.gaggle=$2 AND c.parent_run_id=$3
  AND c.occurrence=$4 AND c.acknowledged_at IS NULL AND c.tombstoned_at IS NULL
  ORDER BY c.sequence LIMIT 2`,
		s.instanceID, grant.Gaggle, grant.ParentRunID, grant.StageOccurrence)
	if err != nil {
		return err
	}
	children, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.ChildRecord, error) { return s.scanChild(row) })
	if err != nil {
		return err
	}
	var stale bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM control.child_lineage WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND occurrence=$4 AND owner_epoch<>$5 AND acknowledged_at IS NULL AND tombstoned_at IS NULL)`, s.instanceID, grant.Gaggle, grant.ParentRunID, grant.StageOccurrence, epoch).Scan(&stale); err != nil {
		return err
	}
	if stale {
		return ErrStaleChildEpoch
	}
	if expected == nil && len(children) == 0 {
		return nil
	}
	if expected == nil || len(children) != 1 {
		return ErrChildObservationChanged
	}
	current := children[0]
	if current.Identity != expected.Identity || current.AcceptanceID != expected.AcceptanceID || current.RunID != expected.RunID || current.ProposalDigest != expected.ProposalDigest || current.StartKey != expected.StartKey || current.State != expected.State || current.CancellationRequested != expected.CancellationRequested {
		return ErrChildObservationChanged
	}
	return nil
}

func sameHandoffReceipt(left, right []byte) bool {
	var l, r journal.ChildHandoffRequest
	return json.Unmarshal(left, &l) == nil && json.Unmarshal(right, &r) == nil && l == r
}
