package claims

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/claimrelease"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

// RecoveryGate reports whether expiry reaping may run for an epoch; the
// localscheduler RecoveryGate satisfies it.
//
// It is an alias, not a defined type, so a consumer declaring the literal
// interface in its own method set (the epoch services' claims seam) is
// satisfied by Plane: method signatures match by type identity.
type RecoveryGate = interface {
	RecoveryPermitted() bool
}

// AdoptAll moves every claim written under an earlier epoch to the handle's
// epoch in one fenced statement. Every older epoch is adopted, not only the
// immediate predecessor: several takeovers can happen between renewals. Plane
// writes adopt lazily anyway; AdoptAll lets a new epoch take custody of every
// live lease before it rebuilds renewals.
func (p *Plane) AdoptAll(ctx context.Context, handle *cloudlease.Handle) (int64, error) {
	var adopted int64
	err := p.fencedWith(ctx, handle, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.claim SET owner_epoch=$2
			WHERE instance_id=$1 AND owner_epoch<$2`,
			p.instanceID, epoch,
		)
		adopted = tag.RowsAffected()
		return err
	})
	return adopted, err
}

// RenewRuns renews every claim whose run is open, mirroring
// localscheduler.ClaimLedger.RenewRuns (renewal.go): each claim of an open run
// takes the RenewEntry path, so a publication lock is never renewed, a live
// lease is extended under the handle's epoch, and an expired lease of an open
// run is granted afresh, as ClaimScoped does. A claim released or taken by
// another run since the read is left alone. It is the rebuild-before-reap pass
// a new epoch runs before its recovery gate opens.
func (p *Plane) RenewRuns(ctx context.Context, handle *cloudlease.Handle, open map[string]bool) ([]localscheduler.ClaimEntry, error) {
	entries, err := p.selectEntries(ctx, p.pool, `true`)
	if err != nil {
		return nil, err
	}
	var candidates []localscheduler.ClaimEntry
	for _, entry := range entries {
		if open[entry.RunID] && !entry.IsPublicationClaim() {
			candidates = append(candidates, entry)
		}
	}
	var renewed []localscheduler.ClaimEntry
	for start := 0; start < len(candidates); start += recoverBatch {
		batch := candidates[start:min(start+recoverBatch, len(candidates))]
		var done []localscheduler.ClaimEntry
		err := p.fencedWith(ctx, handle, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
			done = done[:0]
			for _, entry := range batch {
				ok, err := p.renewRow(ctx, tx, epoch, entry)
				if err != nil {
					return err
				}
				if ok {
					done = append(done, entry)
				}
			}
			return nil
		})
		if err != nil {
			return renewed, err
		}
		renewed = append(renewed, done...)
	}
	return renewed, nil
}

// renewRow renews one claim of an open run if it is still that run's.
func (p *Plane) renewRow(ctx context.Context, tx pgx.Tx, epoch int64, entry localscheduler.ClaimEntry) (bool, error) {
	key := storedKey(entry)
	row, err := p.lockRow(ctx, tx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.entry.RunID != entry.RunID || row.entry.ReleasePending || !row.entry.SharedDeadline.IsZero() {
		return false, nil
	}
	request := httpapi.ClaimRequest{
		Gaggle: key.Gaggle, Provider: key.Provider, ItemID: key.ExternalID,
		RunID: entry.RunID, Workflow: row.entry.Workflow,
	}
	if row.live {
		response, err := p.extend(ctx, tx, epoch, key, row, request, p.defaultLease, true)
		return response.Ok, err
	}
	if err := p.dropExpired(ctx, tx, epoch, key, row); err != nil {
		return false, err
	}
	response, err := p.insert(ctx, tx, epoch, key, request, p.defaultLease)
	return response.Ok, err
}

// RunClosed confirms, from an authoritative source, that a run is closed.
// An error means unknown, never closed. An alias for the same reason as
// RecoveryGate.
type RunClosed = func(ctx context.Context, runID string) (closed bool, err error)

// RecoverExpired releases expired claims as stall-swept, mirroring
// localscheduler.ClaimLedger.RecoverExpired (claim.go), and returns the
// released entries. Two guards narrow it for the cloud:
//
//   - It is a no-op until gate permits recovery: reaping before the epoch's
//     renewal rebuild would hand a live run's item to a second run (DS6).
//     Unlike the local nil-safe gate, a nil gate (typed or not) refuses,
//     because every cloud epoch has a renewal duty.
//   - An expired claim is released only when confirmClosed says its run is
//     closed. Run visibility listings are eventually consistent and omit
//     paused runs, so a run missing from the renewal set may still be live;
//     an error or an open run keeps the claim. A nil confirmClosed refuses.
func (p *Plane) RecoverExpired(
	ctx context.Context, handle *cloudlease.Handle, gate RecoveryGate, confirmClosed RunClosed,
) ([]localscheduler.ClaimEntry, error) {
	if !recoveryPermitted(gate) || confirmClosed == nil {
		return nil, nil
	}
	candidates, err := p.recoveryCandidates(ctx, false)
	if err != nil {
		return nil, err
	}
	closed := map[string]bool{}
	kept := candidates[:0]
	for _, candidate := range candidates {
		runID := candidate.entry.RunID
		isClosed, seen := closed[runID]
		if !seen {
			var confirmErr error
			isClosed, confirmErr = confirmClosed(ctx, runID)
			isClosed = confirmErr == nil && isClosed
			closed[runID] = isClosed
		}
		if isClosed {
			kept = append(kept, candidate)
		}
	}
	return p.releaseCandidates(ctx, handle, kept)
}

// ReleaseRun releases every claim the run holds with reason, under handle's
// fence: the terminal release ClaimLifecycle performs through the ledger's
// ReleaseAllForRun. Publication locks are skipped; their custody ends only by
// verified release or expiry. It returns the released claims, so a second
// call for the same run returns none.
func (p *Plane) ReleaseRun(
	ctx context.Context, handle *cloudlease.Handle, runID string, reason claimrelease.Reason,
) ([]localscheduler.ClaimEntry, error) {
	if runID == "" {
		return nil, errors.New("cloud claims: run release requires a run")
	}
	return p.releaseRun(ctx, handle, runID, "", "", reason)
}

// recoveryPermitted refuses a nil gate, including a typed nil pointer: the
// local *RecoveryGate's nil-safe method permits, which would reap before the
// rebuild.
func recoveryPermitted(gate RecoveryGate) bool {
	if gate == nil {
		return false
	}
	if value := reflect.ValueOf(gate); value.Kind() == reflect.Pointer && value.IsNil() {
		return false
	}
	return gate.RecoveryPermitted()
}

var _ RecoveryGate = (*localscheduler.RecoveryGate)(nil)

// epochClaims restates the epoch services' claims seam with literal parameter
// types, as a consumer outside this package declares it. Plane must satisfy
// it; a defined RecoveryGate or RunClosed type would not.
type epochClaims interface {
	AdoptAll(context.Context, *cloudlease.Handle) (int64, error)
	RenewRuns(context.Context, *cloudlease.Handle, map[string]bool) ([]localscheduler.ClaimEntry, error)
	RecoverExpired(context.Context, *cloudlease.Handle, interface{ RecoveryPermitted() bool },
		func(context.Context, string) (bool, error)) ([]localscheduler.ClaimEntry, error)
	ReleaseRun(context.Context, *cloudlease.Handle, string, claimrelease.Reason) ([]localscheduler.ClaimEntry, error)
}

var _ epochClaims = (*Plane)(nil)
