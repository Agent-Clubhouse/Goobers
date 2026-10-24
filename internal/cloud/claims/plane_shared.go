package claims

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/claimsclient"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

// SharedResolver binds admission and release to trusted run pins. Visibility
// reads the same verified start-time policy for the execution snapshot.
// Incoming requests never select a visibility mode, repository or owner token.
type SharedResolver interface {
	claimsclient.SharedClaimResolver
	Visibility(context.Context, string) (string, error)
}

func sharedRevoked() error { return errors.New("shared execution was administratively revoked") }

// claimShared serializes the whole two-phase transition across replicas, but
// holds the controller fence only for reservation and finalization. A lost
// provider ACK leaves durable pending custody, never execution authority.
func (p *Plane) claimShared(ctx context.Context, handle *cloudlease.Handle, request httpapi.ClaimRequest, ttl time.Duration,
	binding claimsclient.SharedClaimBinding, expected *localscheduler.ClaimEntry,
) (httpapi.ClaimResponse, error) {
	if binding.Store == nil || binding.Owner.Run != request.RunID || binding.Owner.Instance != p.instanceID ||
		binding.RemoteKey != request.ItemID || ttl <= time.Second || ttl > 24*time.Hour {
		return httpapi.ClaimResponse{}, invalidRequest("invalid shared claim binding or TTL")
	}
	key := localscheduler.ClaimKey{Gaggle: request.Gaggle, Provider: request.Provider, ExternalID: request.ItemID}
	if _, err := sharedclaim.Encode(key.ExternalID, sharedclaim.Record{Version: 1, Owner: binding.Owner, ExpiresAt: time.Now()}); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if localscheduler.IsPublicationClaimKey(key.ExternalID) {
		return httpapi.ClaimResponse{}, invalidRequest("publication custody cannot use shared admission")
	}
	var response httpapi.ClaimResponse
	err := p.withCleanupLock(ctx, key, func(ctx context.Context) error {
		if binding.BeforeTransition != nil {
			if err := binding.BeforeTransition(ctx); err != nil {
				return err
			}
		}
		var reserved lockedRow
		var observedAt time.Time
		var admitted bool
		err := p.fencedWith(ctx, handle, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
			var err error
			reserved, admitted, response.Holder, err = p.beginSharedAdmission(ctx, tx, epoch, key, request, binding.Owner, expected)
			if err != nil || !admitted {
				return err
			}
			return tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&observedAt)
		})
		if err != nil || !admitted {
			return err
		}
		// Translate the conservative process-clock deadline into the database
		// clock used by execution snapshots. The sample predates this local
		// instant, so response latency can only shorten execution authority.
		observedLocally := time.Now()
		if binding.AfterTransition != nil {
			defer binding.AfterTransition(ctx)
		}
		deadline, err := sharedclaim.AcquireUntil(ctx, binding.Store, binding.RemoteKey, binding.Owner, ttl)
		if errors.Is(err, sharedclaim.ErrHeld) {
			response.Holder = ""
			return nil
		}
		if err != nil {
			return err
		}
		deadline = observedAt.Add(deadline.Sub(observedLocally))
		return p.fencedWith(ctx, handle, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
			var err error
			response, err = p.finishSharedAdmission(ctx, tx, epoch, key, reserved, deadline)
			return err
		})
	})
	return response, err
}

func (p *Plane) beginSharedAdmission(ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey,
	request httpapi.ClaimRequest, owner sharedclaim.Owner, expected *localscheduler.ClaimEntry,
) (lockedRow, bool, string, error) {
	var revoked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM control.claim_run_history WHERE instance_id=$1 AND run_id=$2
		AND gaggle=$3 AND provider=$4 AND external_id=$5 AND shared_revoked
		UNION ALL SELECT 1 FROM control.claim_history WHERE instance_id=$1 AND run_id=$2
		AND gaggle=$3 AND provider=$4 AND external_id=$5 AND shared_revoked)`,
		p.instanceID, owner.Run, key.Gaggle, key.Provider, key.ExternalID).Scan(&revoked); err != nil {
		return lockedRow{}, false, "", err
	}
	if revoked {
		return lockedRow{}, false, "", sharedRevoked()
	}
	if holder, held, err := p.liveLegacyHolder(ctx, tx, key.ExternalID); err != nil || held {
		return lockedRow{}, false, holder, err
	}
	row, err := p.lockRow(ctx, tx, key)
	held := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return lockedRow{}, false, "", err
	}
	if expected != nil && (!held || row.entry.RunID != expected.RunID || !row.entry.ClaimedAt.Equal(expected.ClaimedAt) ||
		row.entry.SharedOwner != expected.SharedOwner || !row.live) {
		return lockedRow{}, false, row.entry.RunID, nil
	}
	if held && (row.entry.ReleasePending || row.entry.SharedRevoked ||
		(row.entry.SharedOwner != (sharedclaim.Owner{}) && row.entry.SharedOwner != owner) ||
		(row.live && row.entry.RunID != owner.Run)) {
		return lockedRow{}, false, row.entry.RunID, nil
	}
	if held && row.entry.SharedOwner == owner {
		if row.entry.Workflow != request.Workflow {
			return lockedRow{}, false, "", ErrConflict
		}
		tag, updateErr := tx.Exec(ctx, `UPDATE control.claim SET shared_admission_pending=true,owner_epoch=$6
			WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4 AND owner_token=$5 AND owner_epoch<=$6`,
			p.instanceID, key.Gaggle, key.Provider, key.ExternalID, row.token, epoch)
		err = changed(tag.RowsAffected(), updateErr)
	} else {
		if held {
			// Conversion drops local-only authority before any provider I/O.
			tag, deleteErr := tx.Exec(ctx, `DELETE FROM control.claim WHERE instance_id=$1 AND gaggle=$2
				AND provider=$3 AND external_id=$4 AND owner_token=$5 AND owner_epoch<=$6`,
				p.instanceID, key.Gaggle, key.Provider, key.ExternalID, row.token, epoch)
			if err := changed(tag.RowsAffected(), deleteErr); err != nil {
				return lockedRow{}, false, "", err
			}
		}
		if err := p.insertSharedReservation(ctx, tx, epoch, key, request, owner); err != nil {
			return lockedRow{}, false, "", err
		}
	}
	if err != nil {
		return lockedRow{}, false, "", err
	}
	if err := recordGrant(ctx, tx, p.instanceID, key, epoch); err != nil {
		return lockedRow{}, false, "", err
	}
	row, err = p.lockRow(ctx, tx, key)
	return row, err == nil, owner.Run, err
}

func (p *Plane) insertSharedReservation(ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey,
	request httpapi.ClaimRequest, owner sharedclaim.Owner,
) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM control.claim WHERE instance_id=$1
		AND (expires_at>clock_timestamp() OR shared_deadline IS NOT NULL OR release_pending_at IS NOT NULL)`, p.instanceID).Scan(&count); err != nil {
		return err
	}
	if count >= MaxClaims {
		return ErrCapacity
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO control.claim
		(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
		 claimed_at,renewed_at,expires_at,shared_owner,shared_deadline,shared_admission_pending)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now(),now(),now(),$9,now(),true)`,
		p.instanceID, key.Gaggle, key.Provider, key.ExternalID, request.RunID, request.Workflow, token, epoch, raw)
	return err
}

func (p *Plane) finishSharedAdmission(ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey,
	expected lockedRow, deadline time.Time,
) (httpapi.ClaimResponse, error) {
	row, err := p.lockRow(ctx, tx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpapi.ClaimResponse{Ok: false}, nil
	}
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if row.entry.SharedRevoked {
		return httpapi.ClaimResponse{}, sharedRevoked()
	}
	if row.token != expected.token || row.entry.SharedOwner != expected.entry.SharedOwner ||
		!row.entry.SharedAdmissionPending || row.entry.ReleasePending {
		return httpapi.ClaimResponse{Ok: false, Holder: row.entry.RunID}, nil
	}
	// PostgreSQL stores microsecond precision: return exactly what was persisted.
	deadline = deadline.Truncate(time.Microsecond)
	tag, err := tx.Exec(ctx, `UPDATE control.claim
		SET shared_admission_pending=false, shared_deadline=$7, expires_at=$7,renewed_at=clock_timestamp(),owner_epoch=$6
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4 AND owner_token=$5
		AND owner_epoch<=$6 AND $7>clock_timestamp()`,
		p.instanceID, key.Gaggle, key.Provider, key.ExternalID, row.token, epoch, deadline)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if tag.RowsAffected() != 1 {
		return httpapi.ClaimResponse{}, fmt.Errorf("shared admission expired before finalization")
	}
	return p.granted(ctx, tx, epoch, key, row.entry.RunID, deadline)
}
