package claims

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Acquire claims the item for the run. A live lease held by another run (or a
// live legacy claim on the same item) refuses with the holder; a live lease
// the run already holds renews. Merge locks are ordinary exclusive leases: the
// cloud has no host merge-pr flock to bridge.
func (p *Plane) Acquire(ctx context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	if err := requireKey(request); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if request.Workflow == "" {
		return httpapi.ClaimResponse{}, invalidRequest("workflow is required to acquire a claim")
	}
	lease, err := p.leaseDuration(request)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	return p.claim(ctx, request, lease, false)
}

// Renew extends the run's lease, following the ledger's RenewEntry: a renewal
// is a same-run claim, so a key that is free again is granted afresh, while a
// key another run holds answers Ok=false with that holder. Publication locks
// are never renewed; their custody ends by verified release or expiry.
func (p *Plane) Renew(ctx context.Context, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	if err := requireKey(request); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	lease, err := p.leaseDuration(request)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if localscheduler.IsPublicationClaimKey(request.ItemID) {
		return httpapi.ClaimResponse{Ok: false, Holder: request.RunID}, nil
	}
	return p.claim(ctx, request, lease, true)
}

// claim is the shared acquire/renew transition. renewal lets a request
// without a workflow renew a live same-run lease under its recorded workflow.
func (p *Plane) claim(ctx context.Context, request httpapi.ClaimRequest, lease time.Duration, renewal bool) (httpapi.ClaimResponse, error) {
	key := localscheduler.ClaimKey{Gaggle: request.Gaggle, Provider: request.Provider, ExternalID: request.ItemID}
	if p.shared != nil {
		var expected *localscheduler.ClaimEntry
		if renewal {
			entries, err := p.selectEntries(ctx, p.pool, `run_id=$2 AND gaggle=$3 AND provider=$4 AND external_id=$5`,
				request.RunID, key.Gaggle, key.Provider, key.ExternalID)
			if err != nil {
				return httpapi.ClaimResponse{}, err
			}
			if len(entries) > 0 {
				expected = &entries[0]
			}
			if request.Workflow == "" {
				if expected == nil {
					return httpapi.ClaimResponse{Ok: false}, nil
				}
				request.Workflow = expected.Workflow
			}
		}
		binding, err := p.shared.Admission(ctx, key, request.RunID, request.Workflow)
		if err != nil {
			return httpapi.ClaimResponse{}, err
		}
		if binding != nil {
			if renewal && expected == nil {
				return httpapi.ClaimResponse{Ok: false}, nil
			}
			return p.claimShared(ctx, p.owner(), request, lease, *binding, expected)
		}
	}
	var response httpapi.ClaimResponse
	err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if err := pruneHistory(ctx, tx, p.instanceID, epoch); err != nil {
			return err
		}
		if holder, held, err := p.liveLegacyHolder(ctx, tx, request.ItemID); err != nil || held {
			response = httpapi.ClaimResponse{Ok: false, Holder: holder}
			return err
		}
		var err error
		response, err = p.claimRow(ctx, tx, epoch, key, request, lease, renewal)
		return err
	})
	return response, err
}

// claimRow decides the claim against the key's current row.
func (p *Plane) claimRow(
	ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey,
	request httpapi.ClaimRequest, lease time.Duration, renewal bool,
) (httpapi.ClaimResponse, error) {
	row, err := p.lockRow(ctx, tx, key)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return httpapi.ClaimResponse{}, err
	case row.entry.ReleasePending || !row.entry.SharedDeadline.IsZero():
		// Cleanup custody blocks every admission, the run's own included,
		// until FinishProviderRelease (ClaimLedger.claim's transitionPending).
		return httpapi.ClaimResponse{Ok: false, Holder: row.entry.RunID}, nil
	case row.live && row.entry.RunID != request.RunID:
		return httpapi.ClaimResponse{Ok: false, Holder: row.entry.RunID}, nil
	case row.live && row.entry.IsPublicationClaim():
		// Publication custody is exclusive even against its own run: a
		// second acquire while live would hide a lost verdict fence.
		return httpapi.ClaimResponse{Ok: false, Holder: row.entry.RunID}, nil
	case row.live:
		return p.extend(ctx, tx, epoch, key, row, request, lease, renewal)
	default:
		if err := p.dropExpired(ctx, tx, epoch, key, row); err != nil {
			return httpapi.ClaimResponse{}, err
		}
	}
	if request.Workflow == "" {
		// A renewal of a lease that no longer exists has no workflow to
		// grant under; it is stale work for the caller to stop.
		return httpapi.ClaimResponse{Ok: false}, nil
	}
	return p.insert(ctx, tx, epoch, key, request, lease)
}

// extend renews a live lease the run holds, adopting it into this epoch.
func (p *Plane) extend(
	ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey,
	row lockedRow, request httpapi.ClaimRequest, lease time.Duration, renewal bool,
) (httpapi.ClaimResponse, error) {
	if request.Workflow != "" && request.Workflow != row.entry.Workflow {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusConflict, "workflow_mismatch",
			"the run holds this claim under a different workflow", nil)
	}
	if request.Workflow == "" && !renewal {
		return httpapi.ClaimResponse{}, invalidRequest("workflow is required to acquire a claim")
	}
	var expires time.Time
	err := tx.QueryRow(ctx, `
		UPDATE control.claim
		SET renewed_at=clock_timestamp(), expires_at=clock_timestamp()+$7, owner_epoch=$6
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
		  AND owner_token=$5 AND owner_epoch<=$6
		RETURNING expires_at`,
		p.instanceID, key.Gaggle, key.Provider, key.ExternalID, row.token, epoch, lease,
	).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpapi.ClaimResponse{}, errAuthorityMoving(ErrTransition)
	}
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	return p.granted(ctx, tx, epoch, key, request.RunID, expires)
}

// granted records a grant or renewal: the holder's history record and the
// scheduler's fairness state, both in the claim's transaction.
func (p *Plane) granted(
	ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey, runID string, expires time.Time,
) (httpapi.ClaimResponse, error) {
	if err := recordGrant(ctx, tx, p.instanceID, key, epoch); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if err := recordFairnessState(ctx, tx, p.instanceID, key, runID, epoch); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	return httpapi.ClaimResponse{Ok: true, ExpiresAt: &expires}, nil
}

// dropExpired removes an expired row so the key can be granted. Expiry ended
// the old holder's authority; like the file ledger, which overwrites an
// expired entry, it leaves the old holder's history record unreleased.
func (p *Plane) dropExpired(ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey, row lockedRow) error {
	tag, err := tx.Exec(ctx, `
		DELETE FROM control.claim
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
		  AND owner_token=$5 AND owner_epoch<=$6 AND expires_at<=clock_timestamp()`,
		p.instanceID, key.Gaggle, key.Provider, key.ExternalID, row.token, epoch,
	)
	return changed(tag.RowsAffected(), err)
}

// insert grants a new lease with a fresh incarnation token.
func (p *Plane) insert(
	ctx context.Context, tx pgx.Tx, epoch int64, key localscheduler.ClaimKey,
	request httpapi.ClaimRequest, lease time.Duration,
) (httpapi.ClaimResponse, error) {
	var count int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM control.claim
		WHERE instance_id=$1 AND expires_at>clock_timestamp()`,
		p.instanceID,
	).Scan(&count); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	if count >= MaxClaims {
		return httpapi.ClaimResponse{}, httpapi.NewInterventionError(http.StatusServiceUnavailable,
			"claims_capacity", ErrCapacity.Error(), ErrCapacity)
	}
	token, err := randomToken()
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	var expires time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO control.claim
			(instance_id, gaggle, provider, external_id, run_id, workflow,
			 owner_token, owner_epoch, claimed_at, renewed_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp(),clock_timestamp(),clock_timestamp()+$9)
		RETURNING expires_at`,
		p.instanceID, key.Gaggle, key.Provider, key.ExternalID,
		request.RunID, request.Workflow, token, epoch, lease,
	).Scan(&expires); err != nil {
		return httpapi.ClaimResponse{}, err
	}
	return p.granted(ctx, tx, epoch, key, request.RunID, expires)
}

// liveLegacyHolder reports unscoped live or pending custody on the item, which is
// exclusive against every namespace because its namespace is unresolved.
func (p *Plane) liveLegacyHolder(ctx context.Context, tx pgx.Tx, itemID string) (string, bool, error) {
	var holder string
	err := tx.QueryRow(ctx, `
		SELECT run_id FROM control.claim
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$2 AND external_id=$3
		  AND (expires_at>clock_timestamp() OR release_pending_at IS NOT NULL OR shared_admission_pending)`,
		p.instanceID, legacyNamespace, itemID,
	).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return holder, err == nil, err
}

// lockedRow is one claim row read under FOR UPDATE.
type lockedRow struct {
	entry localscheduler.ClaimEntry
	token string
	epoch int64
	live  bool
}

func (p *Plane) lockRow(ctx context.Context, tx pgx.Tx, key localscheduler.ClaimKey) (lockedRow, error) {
	var row lockedRow
	var err error
	row.entry, row.token, row.epoch, row.live, err = scanClaimWithOwnershipAndLive(tx.QueryRow(ctx,
		claimSelectWithOwnership+`,expires_at>clock_timestamp()
		FROM control.claim
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
		FOR UPDATE`,
		p.instanceID, key.Gaggle, key.Provider, key.ExternalID,
	))
	return row, err
}
