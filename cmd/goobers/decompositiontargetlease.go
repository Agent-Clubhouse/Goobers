package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// decompositionTargetLeaseWorkflow is the claims-plane workflow label a
// publication-target lease is recorded under — distinct from the parent
// work item's own select-source claim (which uses the item's ID directly as
// its ExternalID), so releasing a target lease can never affect, or be
// affected by, the parent's own claim state (#4340).
const decompositionTargetLeaseWorkflow = "decomposition-target-lease"

const (
	decompositionTargetLeaseDuration     = 15 * time.Minute
	decompositionTargetLeasePollInterval = 250 * time.Millisecond
)

// decompositionTargetLeaseExternalID derives the target lease's claim key
// from the item id: a distinct namespace, not the bare item id, so this
// lease's ClaimScoped/ReleaseScoped round trips can never collide with — or
// be silently released alongside — that same item's own parent claim. A
// naive first attempt at #4340 shared the parent's own claim key for the
// target lease, so releasing the lease also released the parent claim out
// from under a run still working the batch; this is the fix a parked
// autonomous attempt already found technically correct, just without
// surviving regression coverage for it.
func decompositionTargetLeaseExternalID(itemID string) string {
	return "decomposition-target:" + itemID
}

// claimsPlaneTargetLeaser implements decomposition.TargetLeaser on the
// stage's own claim-ledger seam (openStageClaimLedger): the claims plane in
// a pod, the instance's own claims.json locally — the same abstraction
// every other pod-capable claim already uses. It replaces publish-batch's
// self-only decomposition.FileTargetLeaser and its private
// decomposition-target-locks directory, which a pod cannot reach (#4340).
//
// Acquire POLLS ClaimScoped rather than claiming once and refusing: unlike a
// claim on a work item (refuse, let the engine reschedule the stage later),
// a target lease exists specifically to serialize two concurrent publishers
// of the SAME batch (a self run and a pod run racing each other), and
// FileTargetLeaser's pre-existing contract is to wait for the current
// holder rather than fail the run outright. The wait is always bounded even
// if a holder crashes without releasing: its lease expires after
// leaseDuration, which is the crash-recovery half of #4340's acceptance
// criteria — no manual claims.json cleanup, just the lease's own TTL.
type claimsPlaneTargetLeaser struct {
	layout        instance.Layout
	gaggle        string
	runID         string
	leaseDuration time.Duration
	pollInterval  time.Duration
}

// newDecompositionTargetLeaser constructs the leaser with production timing.
func newDecompositionTargetLeaser(layout instance.Layout, gaggle, runID string) claimsPlaneTargetLeaser {
	return claimsPlaneTargetLeaser{
		layout:        layout,
		gaggle:        gaggle,
		runID:         runID,
		leaseDuration: decompositionTargetLeaseDuration,
		pollInterval:  decompositionTargetLeasePollInterval,
	}
}

// Acquire implements decomposition.TargetLeaser.
func (l claimsPlaneTargetLeaser) Acquire(ctx context.Context, repo providers.RepositoryRef, itemID string) (func() error, error) {
	if itemID == "" {
		return nil, fmt.Errorf("target item id is required")
	}
	instanceLog, closeLog, err := claimLedgerJournal(l.layout)
	if err != nil {
		return nil, err
	}
	ledger, err := openStageClaimLedger(l.layout, withClaimJournal(instanceLog)...)
	if err != nil {
		closeLog()
		return nil, fmt.Errorf("open claim ledger: %w", err)
	}
	key := claimsclient.Key{Gaggle: l.gaggle, Provider: string(repo.Provider), ExternalID: decompositionTargetLeaseExternalID(itemID)}
	ticker := time.NewTicker(l.pollInterval)
	defer ticker.Stop()
	for {
		ok, _, claimErr := ledger.ClaimScoped(ctx, key, l.runID, decompositionTargetLeaseWorkflow, l.leaseDuration)
		if claimErr != nil {
			closeLog()
			return nil, fmt.Errorf("acquire decomposition target lease: %w", claimErr)
		}
		if ok {
			// The instance log stays open for the life of the lease: it is a
			// plain append-only file handle (no exclusive lock is held past
			// journal.OpenInstanceLog's own construction), and the release
			// closure runs long after Acquire returns.
			return func() error {
				defer closeLog()
				return ledger.ReleaseScoped(ctx, key, l.runID)
			}, nil
		}
		select {
		case <-ctx.Done():
			closeLog()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
