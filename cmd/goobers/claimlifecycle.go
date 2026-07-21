package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// releaseClaimsForRun releases every claim owned by runID. It is safe to call
// after an explicit workflow release already did the same work.
func releaseClaimsForRun(l instance.Layout, log *journal.InstanceLog, runID string) error {
	return withClaimLockForRun(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationRunRelease, l.Gaggle(), runID, func() error {
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

// recoverClaims releases expired leases and leases whose owning run is already
// terminal. The latter retries claim cleanup deferred by a claims-lock timeout.
func recoverClaims(l instance.Layout, log *journal.InstanceLog, now time.Time) ([]localscheduler.ClaimEntry, error) {
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	snapshot, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		return nil, err
	}
	var terminalEntries []localscheduler.ClaimEntry
	for _, entry := range snapshot.Snapshot() {
		terminal, err := claimHolderTerminal(l.Root, entry)
		if err != nil {
			recordTerminalClaimInspectionError(log, entry, err)
			continue
		}
		if terminal {
			terminalEntries = append(terminalEntries, entry)
		}
	}

	var released []localscheduler.ClaimEntry
	err = withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationRecovery, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			ledgerPath,
			localscheduler.WithInstanceLog(log),
		)
		if err != nil {
			return err
		}
		expired, err := ledger.RecoverExpired(now)
		if err != nil {
			return err
		}
		released = append(released, expired...)
		for _, entry := range terminalEntries {
			current, held := currentClaimEntry(ledger, entry)
			if !held || current.RunID != entry.RunID {
				continue
			}
			if err := ledger.ReleaseEntry(current, current.RunID); err != nil {
				return fmt.Errorf("release terminal claim %s for run %s: %w", entry.ItemID, entry.RunID, err)
			}
			released = append(released, current)
		}
		return nil
	})
	return released, err
}

func currentClaimEntry(ledger *localscheduler.ClaimLedger, entry localscheduler.ClaimEntry) (localscheduler.ClaimEntry, bool) {
	if entry.Gaggle == "" || entry.Provider == "" {
		return ledger.Lookup(entry.ItemID)
	}
	return ledger.LookupScoped(localscheduler.ClaimKey{
		Gaggle:     entry.Gaggle,
		Provider:   entry.Provider,
		ExternalID: entry.ExternalID,
	})
}

func recordTerminalClaimInspectionError(log *journal.InstanceLog, entry localscheduler.ClaimEntry, err error) {
	if log == nil {
		return
	}
	_ = log.Append(journal.Event{
		Type:     journal.EventError,
		Name:     entry.ItemID,
		Gaggle:   entry.Gaggle,
		Workflow: entry.Workflow,
		RunID:    entry.RunID,
		Error: &journal.ErrorDetail{
			Code:    "terminal_claim_inspection_failed",
			Message: err.Error(),
		},
		Runner: map[string]any{"operation": claimLockOperationRecovery},
	})
}
