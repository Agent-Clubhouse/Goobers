package acceptance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func childAuthorityValid(authority triggerqueue.ChildAuthority) bool {
	return childParentValid(authority.ChildParent) &&
		validText(authority.StageOccurrence, 256, true) &&
		validText(authority.GrantID, 64, true) &&
		validText(authority.AttemptID, 128, true) &&
		blobstore.ValidDigest(authority.ConfigDigest) &&
		blobstore.ValidDigest(authority.PolicyDigest) &&
		!authority.ExpiresAt.IsZero()
}

// BindChildAuthority installs or replaces one exact stage-attempt grant.
func (s *Store) BindChildAuthority(
	ctx context.Context,
	grant triggerqueue.ChildAuthority,
	expectedGrant string,
	now time.Time,
) error {
	if !childAuthorityValid(grant) || grant.Revoked || now.IsZero() ||
		!validText(expectedGrant, 64, false) {
		return triggerqueue.ErrChildAuthorityChanged
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		if err := s.ensureChildParent(txCtx, tx, grant.ChildParent, epoch); err != nil {
			return err
		}
		var cancelled, settled *time.Time
		var parentEpoch int64
		if err := tx.QueryRow(txCtx, `
			SELECT cancelled_at,settled_at,owner_epoch
			FROM control.child_parent
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			FOR UPDATE`,
			s.instanceID, grant.Gaggle, grant.ParentRunID,
		).Scan(&cancelled, &settled, &parentEpoch); err != nil {
			return err
		}
		if parentEpoch != epoch || cancelled != nil {
			return triggerqueue.ErrParentCancelled
		}
		if settled != nil {
			return triggerqueue.ErrParentSettled
		}
		current, err := s.childAuthority(txCtx, tx, grant.ChildParent, grant.StageOccurrence)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if expectedGrant != "" {
				return triggerqueue.ErrChildAuthorityChanged
			}
		case err != nil:
			return err
		default:
			if sameChildAuthority(current, grant) {
				return nil
			}
			if current.GrantID != expectedGrant ||
				grant.GrantID == current.GrantID ||
				(current.Revoked && grant.AttemptID == current.AttemptID) {
				return triggerqueue.ErrChildAuthorityChanged
			}
		}
		tag, err := tx.Exec(txCtx, `
			INSERT INTO control.child_authority (
				instance_id,gaggle,parent_run_id,occurrence,grant_id,attempt_id,
				config_digest,policy_digest,expires_at,revoked,owner_epoch
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,false,$10)
			ON CONFLICT (instance_id,gaggle,parent_run_id,occurrence) DO UPDATE
			SET grant_id=excluded.grant_id,attempt_id=excluded.attempt_id,
			    config_digest=excluded.config_digest,policy_digest=excluded.policy_digest,
			    expires_at=excluded.expires_at,revoked=false,owner_epoch=excluded.owner_epoch
			WHERE control.child_authority.instance_id=$1
			  AND control.child_authority.owner_epoch=$10`,
			s.instanceID, grant.Gaggle, grant.ParentRunID, grant.StageOccurrence,
			grant.GrantID, grant.AttemptID, grant.ConfigDigest, grant.PolicyDigest,
			grant.ExpiresAt.UTC(), epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// ChildAuthority reads retained trusted launcher custody.
func (s *Store) ChildAuthority(
	ctx context.Context,
	parent triggerqueue.ChildParent,
	occurrence string,
) (triggerqueue.ChildAuthority, error) {
	if !childParentValid(parent) || !validText(occurrence, 256, true) {
		return triggerqueue.ChildAuthority{}, triggerqueue.ErrChildAuthorityChanged
	}
	result, err := s.childAuthority(ctx, s.pool, parent, occurrence)
	return result, noRows(err)
}

func (s *Store) childAuthority(
	ctx context.Context,
	query rowQuerier,
	parent triggerqueue.ChildParent,
	occurrence string,
) (triggerqueue.ChildAuthority, error) {
	result := triggerqueue.ChildAuthority{
		ChildParent: parent, StageOccurrence: occurrence,
	}
	err := query.QueryRow(ctx, `
		SELECT grant_id,attempt_id,config_digest,policy_digest,expires_at,revoked
		FROM control.child_authority
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND occurrence=$4`,
		s.instanceID, parent.Gaggle, parent.ParentRunID, occurrence,
	).Scan(
		&result.GrantID, &result.AttemptID, &result.ConfigDigest,
		&result.PolicyDigest, &result.ExpiresAt, &result.Revoked,
	)
	result.ExpiresAt = result.ExpiresAt.UTC()
	return result, err
}

// CheckChildAuthority uses the database clock to reject expiry and replacement.
func (s *Store) CheckChildAuthority(
	ctx context.Context,
	grant triggerqueue.ChildAuthority,
	now time.Time,
) error {
	if !childAuthorityValid(grant) || grant.Revoked || now.IsZero() {
		return triggerqueue.ErrChildAuthorityChanged
	}
	var valid bool
	err := s.pool.QueryRow(ctx, `
		SELECT grant_id=$5 AND attempt_id=$6 AND config_digest=$7
		       AND policy_digest=$8 AND NOT revoked
		       AND expires_at>clock_timestamp()
		FROM control.child_authority
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND occurrence=$4`,
		s.instanceID, grant.Gaggle, grant.ParentRunID, grant.StageOccurrence,
		grant.GrantID, grant.AttemptID, grant.ConfigDigest, grant.PolicyDigest,
	).Scan(&valid)
	if err != nil || !valid {
		return triggerqueue.ErrChildAuthorityChanged
	}
	return nil
}

// CheckChildParentOpen checks cancellation and settlement without allocating.
func (s *Store) CheckChildParentOpen(
	ctx context.Context,
	parent triggerqueue.ChildParent,
) error {
	if !childParentValid(parent) {
		return triggerqueue.ErrChildAuthorityChanged
	}
	var cancelled, settled *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT cancelled_at,settled_at FROM control.child_parent
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3`,
		s.instanceID, parent.Gaggle, parent.ParentRunID,
	).Scan(&cancelled, &settled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if cancelled != nil {
		return triggerqueue.ErrParentCancelled
	}
	if settled != nil {
		return triggerqueue.ErrParentSettled
	}
	return nil
}

// CheckChildAuthorityDelivery also checks the parent family fence.
func (s *Store) CheckChildAuthorityDelivery(
	ctx context.Context,
	grant triggerqueue.ChildAuthority,
	now time.Time,
) error {
	if err := s.CheckChildAuthority(ctx, grant, now); err != nil {
		return err
	}
	return s.CheckChildParentOpen(ctx, grant.ChildParent)
}

func (s *Store) checkChildAuthorityTx(
	ctx context.Context,
	tx pgx.Tx,
	grant triggerqueue.ChildAuthority,
) error {
	var valid bool
	err := tx.QueryRow(ctx, `
		SELECT a.grant_id=$5 AND a.attempt_id=$6 AND a.config_digest=$7
		       AND a.policy_digest=$8 AND NOT a.revoked
		       AND a.expires_at>clock_timestamp()
		       AND p.cancelled_at IS NULL AND p.settled_at IS NULL
		FROM control.child_authority AS a
		JOIN control.child_parent AS p
		  ON p.instance_id=a.instance_id AND p.gaggle=a.gaggle
		 AND p.parent_run_id=a.parent_run_id
		WHERE a.instance_id=$1 AND a.gaggle=$2
		  AND a.parent_run_id=$3 AND a.occurrence=$4`,
		s.instanceID, grant.Gaggle, grant.ParentRunID, grant.StageOccurrence,
		grant.GrantID, grant.AttemptID, grant.ConfigDigest, grant.PolicyDigest,
	).Scan(&valid)
	if err != nil || !valid {
		return triggerqueue.ErrChildAuthorityChanged
	}
	return nil
}

// RevokeChildAuthority fences one exact attempt.
func (s *Store) RevokeChildAuthority(
	ctx context.Context,
	grant triggerqueue.ChildAuthority,
) error {
	if !childAuthorityValid(grant) {
		return triggerqueue.ErrChildAuthorityChanged
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.child_authority SET revoked=true,owner_epoch=$7
			WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3
			  AND occurrence=$4 AND grant_id=$5 AND attempt_id=$6
			  AND owner_epoch=$7`,
			s.instanceID, grant.Gaggle, grant.ParentRunID, grant.StageOccurrence,
			grant.GrantID, grant.AttemptID, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return triggerqueue.ErrChildAuthorityChanged
		}
		return nil
	})
}

// RevokeChildGaggleAuthorities invalidates all existing grants under the fence.
func (s *Store) RevokeChildGaggleAuthorities(ctx context.Context, gaggle string) error {
	if !validText(gaggle, 128, true) {
		return triggerqueue.ErrChildAuthorityChanged
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		_, err := tx.Exec(txCtx, `
			UPDATE control.child_authority
			SET revoked=true,owner_epoch=$3
			WHERE instance_id=$1 AND gaggle=$2
			  AND owner_epoch=$3 AND NOT revoked`,
			s.instanceID, gaggle, epoch,
		)
		return err
	})
}

func sameChildAuthority(left, right triggerqueue.ChildAuthority) bool {
	return left.ChildParent == right.ChildParent &&
		left.StageOccurrence == right.StageOccurrence &&
		left.GrantID == right.GrantID &&
		left.AttemptID == right.AttemptID &&
		left.ConfigDigest == right.ConfigDigest &&
		left.PolicyDigest == right.PolicyDigest &&
		left.ExpiresAt.Equal(right.ExpiresAt) &&
		left.Revoked == right.Revoked
}
