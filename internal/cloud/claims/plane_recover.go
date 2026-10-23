package claims

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// recoverBatch bounds the rows one fenced recovery transaction releases, so
// each stays far inside the two-second fence.
const recoverBatch = 100

// recoveryCandidate is a lease the sweep may release.
type recoveryCandidate struct {
	entry   localscheduler.ClaimEntry
	token   string
	expired bool
	reason  claimrelease.Reason
}

// Recover is the stale-claim sweep: it releases every expired lease (as
// stall-swept) and every lease whose run's journal is terminal (with the
// run's own terminal reason). Publication locks end only by expiry. The
// decision and the run journals are read outside the fence; each release
// then pins the row's incarnation, and an expiry release rechecks expiry, so
// a lease renewed meanwhile survives. The HTTP recovery gate keeps the sweep
// from running before the controller has finished taking over.
func (p *Plane) Recover(ctx context.Context, _ httpapi.ClaimRecoverRequest) (httpapi.ClaimRecoverResponse, error) {
	candidates, err := p.recoveryCandidates(ctx)
	if err != nil {
		return httpapi.ClaimRecoverResponse{}, err
	}
	var released []localscheduler.ClaimEntry
	for start := 0; start < len(candidates); start += recoverBatch {
		batch := candidates[start:min(start+recoverBatch, len(candidates))]
		var moved []localscheduler.ClaimEntry
		err := p.fenced(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
			moved = moved[:0]
			for _, candidate := range batch {
				ok, err := p.moveToHistory(ctx, tx, epoch, storedKey(candidate.entry),
					candidate.entry.RunID, candidate.token, candidate.expired, candidate.reason)
				if err != nil {
					return err
				}
				if ok {
					moved = append(moved, candidate.entry)
				}
			}
			return nil
		})
		if err != nil {
			return httpapi.ClaimRecoverResponse{Released: wireEntries(released)}, err
		}
		released = append(released, moved...)
	}
	return httpapi.ClaimRecoverResponse{Released: wireEntries(released)}, nil
}

// recoveryCandidates reads expired leases and leases held by terminal runs,
// with the reason each release will record.
func (p *Plane) recoveryCandidates(ctx context.Context) ([]recoveryCandidate, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT c.external_id,c.gaggle,c.provider,c.run_id,c.workflow,
		       c.claimed_at,c.renewed_at,c.expires_at,c.verification,c.owner_token,
		       c.expires_at<=clock_timestamp(), coalesce(k.terminal,false)
		FROM control.claim AS c
		LEFT JOIN journal.run_checkpoint AS k ON k.instance_id=c.instance_id AND k.run_id=c.run_id
		WHERE c.instance_id=$1 AND (c.expires_at<=clock_timestamp() OR k.terminal)
		ORDER BY c.gaggle,c.provider,c.external_id
		LIMIT $2`,
		p.instanceID, MaxClaims,
	)
	if err != nil {
		return nil, err
	}
	var candidates []recoveryCandidate
	terminalRuns := map[string]bool{}
	for rows.Next() {
		var candidate recoveryCandidate
		var verification []byte
		var terminal bool
		err := rows.Scan(
			&candidate.entry.ItemID, &candidate.entry.Gaggle, &candidate.entry.Provider,
			&candidate.entry.RunID, &candidate.entry.Workflow, &candidate.entry.ClaimedAt,
			&candidate.entry.RenewedAt, &candidate.entry.ExpiresAt, &verification,
			&candidate.token, &candidate.expired, &terminal,
		)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidate.entry.ExternalID = candidate.entry.ItemID
		if len(verification) > 0 {
			if err := json.Unmarshal(verification, &candidate.entry.Verification); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if !candidate.expired && candidate.entry.IsPublicationClaim() {
			continue // publication custody ends by verified release or expiry
		}
		if !candidate.expired {
			terminalRuns[candidate.entry.RunID] = true
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return p.withReasons(ctx, candidates, terminalRuns)
}

// withReasons attaches each candidate's release reason: stall-swept for
// expiry, otherwise the terminal reason derived from the run's own journal.
func (p *Plane) withReasons(ctx context.Context, candidates []recoveryCandidate, terminalRuns map[string]bool) ([]recoveryCandidate, error) {
	reasons := make(map[string]claimrelease.Reason, len(terminalRuns))
	for runID := range terminalRuns {
		reason, err := p.terminalReason(ctx, runID)
		if err != nil {
			return nil, err
		}
		reasons[runID] = reason
	}
	for i := range candidates {
		if candidates[i].expired {
			candidates[i].reason = localscheduler.StallSweptReason()
		} else {
			candidates[i].reason = reasons[candidates[i].entry.RunID]
		}
	}
	return candidates, nil
}

// terminalReason derives the run's release reason from its journal events,
// the same derivation the daemon's terminal recovery uses.
func (p *Plane) terminalReason(ctx context.Context, runID string) (claimrelease.Reason, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT body FROM journal.run_event WHERE instance_id=$1 AND run_id=$2 ORDER BY seq`,
		p.instanceID, runID,
	)
	if err != nil {
		return claimrelease.Reason{}, err
	}
	defer rows.Close()
	var events []journal.Event
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return claimrelease.Reason{}, err
		}
		var event journal.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return claimrelease.Unknown.Because(localscheduler.ReleaseCodeRunJournalUnavailable, "").Normalize(), nil
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return claimrelease.Reason{}, err
	}
	return localscheduler.ReleaseReasonFromRunEvents(events), nil
}
