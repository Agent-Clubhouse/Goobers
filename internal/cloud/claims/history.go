package claims

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Claim history mirrors localscheduler.ClaimLedger's: one record per run and
// key (history[runID][storageKey]). storeClaim records the lease on every
// grant and renewal, RecordClaimVerification records the observation, and a
// release (recordReleasedHistory) completes the same record with its release
// time and reason. Overwriting an expired lease leaves its owner's record
// untouched, unreleased.

const historyColumns = `instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
	claimed_at,renewed_at,expires_at,released_at,release_reason,verification`

// historyUpsert replaces the run and key's record with the incoming state.
const historyUpsert = `
	ON CONFLICT (instance_id,run_id,gaggle,provider,external_id) DO UPDATE
	SET workflow=excluded.workflow, owner_token=excluded.owner_token, owner_epoch=excluded.owner_epoch,
	    claimed_at=excluded.claimed_at, renewed_at=excluded.renewed_at, expires_at=excluded.expires_at,
	    released_at=excluded.released_at, release_reason=excluded.release_reason,
	    verification=excluded.verification`

// recordGrant records the key's current lease as its holder's history, the
// ledger's storeClaim/recordHistory.
func recordGrant(ctx context.Context, tx pgx.Tx, instanceID string, key localscheduler.ClaimKey, epoch int64) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO control.claim_history (`+historyColumns+`)
		SELECT instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,$5,
		       claimed_at,renewed_at,expires_at,NULL,NULL,verification
		FROM control.claim
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4`+historyUpsert,
		instanceID, key.Gaggle, key.Provider, key.ExternalID, epoch,
	)
	return changed(tag.RowsAffected(), err)
}

// releaseToHistory deletes the run's lease for key and completes its history
// record with the release, the ledger's recordReleasedHistory. token, when
// set, pins one incarnation; requireExpired rechecks expiry under the fence.
func releaseToHistory(
	ctx context.Context, tx pgx.Tx, instanceID string, epoch int64, key localscheduler.ClaimKey,
	runID, token string, requireExpired bool, reason claimrelease.Reason,
) (bool, error) {
	rawReason, err := json.Marshal(reason)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		WITH moved AS (
			DELETE FROM control.claim
			WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
			  AND run_id=$5 AND owner_epoch<=$6 AND ($8='' OR owner_token=$8)
			  AND (NOT $9 OR expires_at<=clock_timestamp())
			RETURNING *
		)
		INSERT INTO control.claim_history (`+historyColumns+`)
		SELECT instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,$6,
		       claimed_at,renewed_at,expires_at,clock_timestamp(),$7,verification
		FROM moved`+historyUpsert,
		instanceID, key.Gaggle, key.Provider, key.ExternalID, runID, epoch, rawReason, token, requireExpired,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// historyRetained is the ledger's retainedHistory: a run's records are kept
// while it holds any lease, and otherwise until its newest activity (release,
// else last grant) is older than ClaimHistoryTTL. $1 is the instance and the
// TTL parameter is substituted by the caller.
const historyRetained = `(EXISTS (
		SELECT 1 FROM control.claim AS held WHERE held.instance_id=h.instance_id AND held.run_id=h.run_id
	) OR h.run_id IN (
		SELECT run_id FROM control.claim_history WHERE instance_id=$1 GROUP BY run_id
		HAVING max(coalesce(released_at, greatest(claimed_at, renewed_at))) > clock_timestamp()-%s::interval
	))`

// Orders of the ledger's two history reads.
const (
	// historyByKey is HistoryForRun's order.
	historyByKey = `ORDER BY h.gaggle,h.provider,h.external_id`
	// historyNewestFirst is HistorySnapshot's order: release time, else grant
	// time, newest first; then run and item.
	historyNewestFirst = `ORDER BY coalesce(h.released_at,h.claimed_at) DESC,h.run_id,h.external_id`
)

// pruneHistory drops the records retention no longer keeps.
func pruneHistory(ctx context.Context, tx pgx.Tx, instanceID string, epoch int64) error {
	_, err := tx.Exec(ctx, `
		DELETE FROM control.claim_history AS h
		WHERE h.instance_id=$1 AND h.owner_epoch<=$2 AND NOT `+retainedWith("$3"),
		instanceID, epoch, localscheduler.ClaimHistoryTTL,
	)
	return err
}

// retainedWith instantiates historyRetained with the TTL parameter's index.
func retainedWith(ttlParam string) string {
	return fmt.Sprintf(historyRetained, ttlParam)
}

const claimHistorySelectAliased = `SELECT h.external_id,h.gaggle,h.provider,h.run_id,h.workflow,
	h.claimed_at,h.renewed_at,h.expires_at,h.verification,h.released_at,h.release_reason`

// queryHistory reads retained history records matching predicate (over alias
// h, with args from $2) in the given order.
func queryHistory(ctx context.Context, q querier, instanceID, predicate, order string, args ...any) ([]localscheduler.ClaimEntry, error) {
	ttl := "$" + argIndex(len(args)+2)
	rows, err := q.Query(ctx, claimHistorySelectAliased+`
		FROM control.claim_history AS h
		WHERE h.instance_id=$1 AND `+predicate+` AND `+retainedWith(ttl)+`
		`+order,
		append(append([]any{instanceID}, args...), localscheduler.ClaimHistoryTTL)...,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanHistoryClaim)
}
