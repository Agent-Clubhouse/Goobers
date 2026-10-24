package claims

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

// MarkerReleaser removes the provider-visible claim marker for claim, held by
// claim.RunID, with daemon-held credentials resolved from the run's trusted
// identity. It must be idempotent (an absent marker is success) and must
// refuse, never close, a successor's claim. A refusal retrying cannot fix is
// wrapped in ErrProviderCleanupRefused.
type MarkerReleaser func(ctx context.Context, claim localscheduler.ClaimEntry, reason claimrelease.Reason) error

// ErrProviderCleanupRefused marks a marker release that retrying cannot fix
// (a successor holds the item, the repository is not pinned for the run).
var ErrProviderCleanupRefused = errors.New("cloud claims: provider cleanup refused")

// providerCleanup is the ledger's ReleaseWithProvider (BeginProviderRelease,
// cleanup, FinishProviderRelease) for one exact claim incarnation. Custody is
// reserved durably first, so a crash after the provider call leaves the claim
// held and its cleanup retryable; no one else may take, renew or release it
// meanwhile. The provider call runs outside the fence, serialized per key.
func (p *Plane) providerCleanup(ctx context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	if p.releaseMarker == nil {
		return httpapi.ClaimResponse{}, errProviderCleanupUnavailable()
	}
	if err := requireKey(request); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if request.ClaimedAt.IsZero() {
		return httpapi.ClaimResponse{}, invalidRequest("provider cleanup requires the claim incarnation (claimedAt)")
	}
	if localscheduler.IsPublicationClaimKey(request.ItemID) {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusConflict, "publication_custody",
			"publication custody cannot use provider cleanup", nil)
	}
	reason := request.Reason.Normalize()
	key := localscheduler.ClaimKey{Gaggle: request.Gaggle, Provider: request.Provider, ExternalID: request.ItemID}
	held, reserved, err := p.beginCleanup(ctx, key, request)
	if err != nil || !reserved {
		return httpapi.ClaimResponse{Ok: false}, err
	}
	if err := p.withCleanupLock(ctx, key, func(ctx context.Context) error {
		return p.releaseMarker(ctx, held.entry, reason)
	}); err != nil {
		return httpapi.ClaimResponse{}, cleanupError(err)
	}
	var released bool
	err = p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		var err error
		released, err = releaseToHistory(ctx, tx, p.instanceID, epoch, key, request.RunID, held.token, false, true, reason)
		return err
	})
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	return httpapi.ClaimResponse{Ok: released}, nil
}

// beginCleanup reserves cleanup custody of the run's exact incarnation, the
// ledger's BeginProviderRelease: another incarnation, or a claim the run no
// longer holds, reserves nothing (Ok=false). A reservation already held
// continues, so a retried cleanup resumes it.
func (p *Plane) beginCleanup(ctx context.Context, key localscheduler.ClaimKey, request httpapi.ClaimRequest) (lockedRow, bool, error) {
	var held lockedRow
	reserved := false
	err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		row, err := p.lockRow(ctx, tx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.entry.RunID != request.RunID || !row.entry.ClaimedAt.Equal(request.ClaimedAt) {
			return nil
		}
		held, reserved = row, true
		if row.entry.ReleasePending {
			return nil
		}
		tag, err := tx.Exec(ctx, `
			UPDATE control.claim SET release_pending_at=clock_timestamp(), owner_epoch=$6
			WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
			  AND owner_token=$5 AND owner_epoch<=$6 AND release_pending_at IS NULL`,
			p.instanceID, key.Gaggle, key.Provider, key.ExternalID, row.token, epoch,
		)
		if err := changed(tag.RowsAffected(), err); err != nil {
			return err
		}
		held.entry.ReleasePending = true
		// The ledger records custody on the run's history record too.
		return recordGrant(ctx, tx, p.instanceID, key, epoch)
	})
	return held, reserved, err
}

// withCleanupLock serializes cleanup attempts for one key across replicas,
// like the ledger's per-item provider-release lock, for the duration of the
// provider call only. A transaction-scoped lock works through a transaction
// pooler and is released if the session dies.
func (p *Plane) withCleanupLock(ctx context.Context, key localscheduler.ClaimKey, cleanup func(context.Context) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('claim-cleanup:' || $1, 0))`,
		p.instanceID+"|"+key.Gaggle+"|"+key.Provider+"|"+key.ExternalID); err != nil {
		return err
	}
	return cleanup(ctx)
}

// cleanupError keeps custody and reports why: a refusal is permanent (409),
// anything else is a retryable provider failure.
func cleanupError(err error) error {
	if errors.Is(err, ErrProviderCleanupRefused) {
		return httpapi.NewInterventionError(http.StatusConflict, "provider_cleanup_refused",
			"provider cleanup was refused; the claim stays under cleanup custody", err)
	}
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "provider_cleanup_failed",
		"provider cleanup failed; the claim stays under cleanup custody, retry the release", err)
}
