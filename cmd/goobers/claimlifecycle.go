package main

import (
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// releaseClaimsForRun releases every claim owned by runID. It is safe to call
// after an explicit workflow release already did the same work.
func releaseClaimsForRun(l instance.Layout, log *journal.InstanceLog, runID string) error {
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationRunRelease, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(l.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithInstanceLog(log),
		)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		for _, entry := range ledger.ForRunAll(runID) {
			if err := ledger.ReleaseEntry(entry, runID); err != nil {
				return fmt.Errorf("release claim %s for run %s: %w", entry.ItemID, runID, err)
			}
		}
		return nil
	})
}
