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
//
// Records live in control.claim_run_history. Every release is also appended
// to control.claim_history, one row per released incarnation, exactly as the
// previous binary writes it, so that binary can still read and release during
// a rolling upgrade (migration 0022 is an expand). Reads merge the two and
// keep each run and key's newest activity, so a release the previous binary
// made still completes the run's record.

const runHistoryColumns = `instance_id,run_id,gaggle,provider,external_id,workflow,owner_token,owner_epoch,
	claimed_at,renewed_at,expires_at,released_at,release_reason,verification,release_pending_at`

// runHistoryUpsert replaces the run and key's record with the incoming state.
const runHistoryUpsert = `
	ON CONFLICT (instance_id,run_id,gaggle,provider,external_id) DO UPDATE
	SET workflow=excluded.workflow, owner_token=excluded.owner_token, owner_epoch=excluded.owner_epoch,
	    claimed_at=excluded.claimed_at, renewed_at=excluded.renewed_at, expires_at=excluded.expires_at,
	    released_at=excluded.released_at, release_reason=excluded.release_reason,
	    verification=excluded.verification, release_pending_at=excluded.release_pending_at`

// recordGrant records the key's current lease as its holder's history, the
// ledger's storeClaim/recordHistory.
func recordGrant(ctx context.Context, tx pgx.Tx, instanceID string, key localscheduler.ClaimKey, epoch int64) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO control.claim_run_history (`+runHistoryColumns+`)
		SELECT instance_id,run_id,gaggle,provider,external_id,workflow,owner_token,$5,
		       claimed_at,renewed_at,expires_at,NULL,NULL,verification,release_pending_at
		FROM control.claim
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4`+runHistoryUpsert,
		instanceID, key.Gaggle, key.Provider, key.ExternalID, epoch,
	)
	return changed(tag.RowsAffected(), err)
}

// releaseToHistory deletes the run's lease for key, appends the released
// incarnation to control.claim_history as the previous binary does, and
// completes the run's record (recordReleasedHistory), all at one release
// instant. token, when set, pins one incarnation; requireExpired rechecks
// expiry under the fence. custody finishes a provider-cleanup release: only a
// row under cleanup custody is released, while an ordinary release never
// discards custody (the ledger's ErrClaimReleasePending).
func releaseToHistory(
	ctx context.Context, tx pgx.Tx, instanceID string, epoch int64, key localscheduler.ClaimKey,
	runID, token string, requireExpired, custody bool, reason claimrelease.Reason,
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
			  AND (release_pending_at IS NOT NULL) = $10
			RETURNING instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,
			          claimed_at,renewed_at,expires_at,verification,clock_timestamp() AS released
		), incarnation AS (
			INSERT INTO control.claim_history
				(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
				 claimed_at,renewed_at,expires_at,released_at,release_reason,verification)
			SELECT instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,$6,
			       claimed_at,renewed_at,expires_at,released,$7,verification
			FROM moved
		)
		INSERT INTO control.claim_run_history (`+runHistoryColumns+`)
		SELECT instance_id,run_id,gaggle,provider,external_id,workflow,owner_token,$6,
		       claimed_at,renewed_at,expires_at,released,$7,verification,NULL
		FROM moved`+runHistoryUpsert,
		instanceID, key.Gaggle, key.Provider, key.ExternalID, runID, epoch, rawReason, token, requireExpired, custody,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// pruneHistory applies each store's retention: the previous binary's rule for
// released incarnations, and the ledger's retainedHistory for run records (a
// run holding any lease keeps every record; otherwise its records go once its
// newest activity across both stores, release else last grant, passes
// ClaimHistoryTTL).
func pruneHistory(ctx context.Context, tx pgx.Tx, instanceID string, epoch int64) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM control.claim_history
		WHERE instance_id=$1 AND owner_epoch<=$2
		  AND released_at<(clock_timestamp()-$3::interval)`,
		instanceID, epoch, localscheduler.ClaimHistoryTTL,
	); err != nil {
		return err
	}
	// A run's newest activity is taken over the same merge history reads use:
	// a release the previous binary appended to claim_history keeps the run's
	// records, so retention never drops records a merged read still keeps.
	_, err := tx.Exec(ctx, `
		DELETE FROM control.claim_run_history AS h
		WHERE h.instance_id=$1 AND h.owner_epoch<=$2
		  AND NOT EXISTS (SELECT 1 FROM control.claim AS held WHERE held.instance_id=$1 AND held.run_id=h.run_id)
		  AND h.run_id IN (
			SELECT run_id FROM (
				SELECT run_id, coalesce(released_at, greatest(claimed_at, renewed_at)) AS activity
				FROM control.claim_run_history WHERE instance_id=$1
				UNION ALL
				SELECT run_id, released_at FROM control.claim_history WHERE instance_id=$1
			) AS merged
			GROUP BY run_id
			HAVING max(activity) <= clock_timestamp()-$3::interval
		  )`,
		instanceID, epoch, localscheduler.ClaimHistoryTTL,
	)
	return err
}

// historyRecords is each run and key's newest record across both stores,
// with its activity time (release, else last grant). On a tie the run record
// wins; the two then carry the same release.
const historyRecords = `WITH merged AS (
		SELECT run_id,gaggle,provider,external_id,workflow,claimed_at,renewed_at,expires_at,
		       verification,released_at,release_reason,release_pending_at IS NOT NULL AS release_pending,
		       coalesce(released_at, greatest(claimed_at, renewed_at)) AS activity, 0 AS source
		FROM control.claim_run_history WHERE instance_id=$1
		UNION ALL
		SELECT run_id,gaggle,provider,external_id,workflow,claimed_at,renewed_at,expires_at,
		       verification,released_at,release_reason,false,released_at, 1
		FROM control.claim_history WHERE instance_id=$1
	), records AS (
		SELECT DISTINCT ON (run_id,gaggle,provider,external_id) *
		FROM merged ORDER BY run_id,gaggle,provider,external_id,activity DESC,source
	)`

// historyRetained is the ledger's retainedHistory over the merged records:
// a run holding any lease keeps every record, otherwise its records are kept
// while its newest activity is within the TTL (%s is the TTL parameter).
const historyRetained = `(EXISTS (
		SELECT 1 FROM control.claim AS held WHERE held.instance_id=$1 AND held.run_id=h.run_id
	) OR h.run_id IN (
		SELECT run_id FROM records GROUP BY run_id HAVING max(activity) > clock_timestamp()-%s::interval
	))`

// Orders of the ledger's two history reads.
const (
	// historyByKey is HistoryForRun's order.
	historyByKey = `ORDER BY h.gaggle,h.provider,h.external_id`
	// historyNewestFirst is HistorySnapshot's order: release time, else grant
	// time, newest first; then run and item.
	historyNewestFirst = `ORDER BY coalesce(h.released_at,h.claimed_at) DESC,h.run_id,h.external_id`
)

// queryHistory reads retained history records matching predicate (over alias
// h, with args from $2) in the given order.
func queryHistory(ctx context.Context, q querier, instanceID, predicate, order string, args ...any) ([]localscheduler.ClaimEntry, error) {
	ttl := "$" + argIndex(len(args)+2)
	rows, err := q.Query(ctx, historyRecords+`
		SELECT h.external_id,h.gaggle,h.provider,h.run_id,h.workflow,
		       h.claimed_at,h.renewed_at,h.expires_at,h.verification,h.released_at,h.release_reason,
		       h.release_pending
		FROM records AS h
		WHERE `+predicate+` AND `+fmt.Sprintf(historyRetained, ttl)+`
		`+order,
		append(append([]any{instanceID}, args...), localscheduler.ClaimHistoryTTL)...,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanHistoryClaim)
}
