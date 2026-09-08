package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// authorizeRecoveryDelivery checks the current lease and durable issue identity
// for an authenticated receiving run. runID must come from authentication, not
// an untrusted request field. The returned deadline bounds the transfer context;
// this does not authorize arbitrary cross-run reads or bypass archive selection.
func authorizeRecoveryDelivery(ctx context.Context, layout instance.Layout, runID, repositoryKey, issueID string, now time.Time) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		return time.Time{}, err
	}
	matched, err := recoveryClaimMatches(events, runID, repositoryKey, issueID)
	if err != nil {
		return time.Time{}, err
	}
	if !matched {
		return time.Time{}, fmt.Errorf("recovery delivery requires a matching recorded issue claim")
	}
	var deadline time.Time
	err = withClaimLock(filepath.Join(layout.SchedulerDir(), claimLockFileName), claimLockOperationRunLookup, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return err
		}
		claims := ledger.ForRunAll(runID)
		if len(claims) != 1 || claims[0].ItemID != issueID || claims[0].ReleasedAt != nil || !claims[0].ExpiresAt.After(now) {
			return fmt.Errorf("recovery delivery requires exactly one current unexpired issue lease")
		}
		deadline = claims[0].ExpiresAt
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return deadline, nil
}
