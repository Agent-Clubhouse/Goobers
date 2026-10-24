package claims

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/claimrelease"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

// sharedRequest finds persisted custody; release must never reinterpret it
// using the current configuration or a caller-supplied owner.
func (p *Plane) sharedRequest(ctx context.Context, request httpapi.ClaimRequest) (*localscheduler.ClaimEntry, error) {
	entries, err := p.selectEntries(ctx, p.pool, `run_id=$2 AND gaggle=$3 AND provider=$4 AND external_id=$5`,
		request.RunID, request.Gaggle, request.Provider, request.ItemID)
	if err != nil || len(entries) == 0 || entries[0].SharedDeadline.IsZero() {
		return nil, err
	}
	return &entries[0], nil
}

func (p *Plane) releaseShared(ctx context.Context, handle *cloudlease.Handle, expected localscheduler.ClaimEntry,
	reason claimrelease.Reason, expired bool,
) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if p.shared == nil {
		return false, errors.New("shared release requires its pinned provider resolver")
	}
	// Revocation must win over an in-flight CAS without waiting for provider
	// I/O. Cleanup custody is retained even when provider credentials fail.
	if reason.Disposition == claimrelease.ForceReleased {
		if err := p.revokeShared(ctx, handle, expected, reason.Actor); err != nil {
			return false, err
		}
	}
	key := storedKey(expected)
	var released bool
	err := p.withCleanupLock(ctx, key, func(ctx context.Context) error {
		binding, err := p.shared.Release(ctx, expected)
		if err != nil {
			return err
		}
		if binding.Store == nil || binding.Owner != expected.SharedOwner || binding.RemoteKey != key.ExternalID {
			return errors.New("shared release differs from persisted ownership")
		}
		if binding.BeforeTransition != nil {
			if err := binding.BeforeTransition(ctx); err != nil {
				return err
			}
		}
		var held lockedRow
		var reserved bool
		err = p.fencedWith(ctx, handle, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
			var err error
			held, reserved, err = p.beginSharedRelease(ctx, tx, epoch, expected, expired)
			return err
		})
		if err != nil || !reserved {
			return err
		}
		if binding.AfterTransition != nil {
			defer binding.AfterTransition(ctx)
		}
		err = sharedclaim.Release(ctx, binding.Store, binding.RemoteKey, binding.Owner)
		if errors.Is(err, sharedclaim.ErrNotOwner) {
			err = sharedclaim.ConfirmOwnerGone(ctx, binding.Store, binding.RemoteKey, binding.Owner)
		}
		if err != nil {
			return err
		}
		return p.fencedWith(ctx, handle, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
			var err error
			released, err = releaseToHistory(ctx, tx, p.instanceID, epoch, key, expected.RunID,
				held.token, false, true, reason.Normalize())
			return err
		})
	})
	return released, err
}

func (p *Plane) beginSharedRelease(ctx context.Context, tx pgx.Tx, epoch int64,
	expected localscheduler.ClaimEntry, expired bool,
) (lockedRow, bool, error) {
	key := storedKey(expected)
	row, err := p.lockRow(ctx, tx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedRow{}, false, nil
	}
	if err != nil {
		return lockedRow{}, false, err
	}
	if row.entry.RunID != expected.RunID || row.entry.SharedOwner != expected.SharedOwner ||
		!row.entry.ClaimedAt.Equal(expected.ClaimedAt) || (expired && row.live) {
		return lockedRow{}, false, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE control.claim SET release_pending_at=coalesce(release_pending_at,clock_timestamp()),owner_epoch=$6
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4 AND owner_token=$5 AND owner_epoch<=$6`,
		p.instanceID, key.Gaggle, key.Provider, key.ExternalID, row.token, epoch)
	if err := changed(tag.RowsAffected(), err); err != nil {
		return lockedRow{}, false, err
	}
	return row, true, recordGrant(ctx, tx, p.instanceID, key, epoch)
}

func (p *Plane) revokeShared(ctx context.Context, handle *cloudlease.Handle, expected localscheduler.ClaimEntry, actor string) error {
	if actor == "" || expected.SharedOwner == (sharedclaim.Owner{}) {
		return invalidRequest("shared revocation requires an actor and persisted ownership")
	}
	return p.fencedWith(ctx, handle, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		key := storedKey(expected)
		row, err := p.lockRow(ctx, tx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.entry.SharedOwner != expected.SharedOwner || !row.entry.ClaimedAt.Equal(expected.ClaimedAt) {
			return sharedclaim.ErrNotOwner
		}
		tag, err := tx.Exec(ctx, `UPDATE control.claim SET shared_revoked=true,
			shared_deadline=least(shared_deadline,now()),expires_at=least(shared_deadline,now()),owner_epoch=$6
			WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4 AND owner_token=$5 AND owner_epoch<=$6`,
			p.instanceID, key.Gaggle, key.Provider, key.ExternalID, row.token, epoch)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		return recordGrant(ctx, tx, p.instanceID, key, epoch)
	})
}
