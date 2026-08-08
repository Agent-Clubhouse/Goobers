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
		return releaseClaimsForRunLocked(l, log, runID)
	})
}

func releaseClaimsForRunWithDefaultTimeout(l instance.Layout, log *journal.InstanceLog, runID string) error {
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	return withClaimLockBounds(lockPath, claimLockOperationRunRelease, instance.DefaultClaimsLockTimeout, claimLockSlowThreshold, claimLockEventContext{
		Gaggle: l.Gaggle(),
		RunID:  runID,
	}, func() error {
		return releaseClaimsForRunLocked(l, log, runID)
	})
}

func releaseClaimsForRunLocked(l instance.Layout, log *journal.InstanceLog, runID string) error {
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
}

// renewLiveClaims re-acquires every claim held by a run this process is
// currently tracking (daemonRunnerRegistry.RunIDs, via `goobers up`'s
// periodic claimTicker) — issue #2014's liveness signal, chosen over a
// per-stage heartbeat because a stage runs as a subprocess with no reach into
// the daemon process's claim ledger, while this process's own bookkeeping of
// "which runs am I actively driving right now" already exists (stalledruns.go)
// and stops tracking a run the moment it finishes or this process dies —
// exactly the liveness RecoverExpired's own doc has always wanted and never
// had. Opens the ledger WITHOUT WithInstanceLog deliberately: RenewEntry
// reuses Claim/ClaimScoped, which journals claim.acquired on every success,
// and a routine renewal every claimRecoverInterval for every live run is
// bookkeeping, not an audit-worthy event — journaling it here would add a
// recurring InstanceLog.Append (a real cost per its own doc) purely to
// restate what the original claim and any eventual release already record.
func renewLiveClaims(l instance.Layout, runIDs []string, leaseDuration time.Duration) ([]localscheduler.ClaimEntry, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	var renewed []localscheduler.ClaimEntry
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationRenewal, func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		for _, runID := range runIDs {
			for _, entry := range ledger.ForRunAll(runID) {
				ok, err := ledger.RenewEntry(entry, leaseDuration)
				if err != nil {
					return fmt.Errorf("renew claim %s for run %s: %w", entry.ItemID, runID, err)
				}
				if ok {
					renewed = append(renewed, entry)
				}
			}
		}
		return nil
	})
	return renewed, err
}

// recoverClaims releases expired leases and leases whose owning run is already
// terminal. The latter retries claim cleanup deferred by a claims-lock timeout.
func recoverClaims(
	l instance.Layout,
	log *journal.InstanceLog,
	now time.Time,
	interventionActive func(string) bool,
) ([]localscheduler.ClaimEntry, error) {
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	snapshot, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		return nil, err
	}
	var terminalEntries []localscheduler.ClaimEntry
	terminalUpdates := make(map[string]remediationNoopUpdate)
	for _, entry := range snapshot.Snapshot() {
		terminal, err := claimHolderTerminal(l.Root, entry)
		if err != nil {
			recordTerminalClaimInspectionError(log, entry, err)
			continue
		}
		if terminal {
			terminalEntries = append(terminalEntries, entry)
			if _, isReconcileClaim := parseBacklogReconcileRunID(entry.RunID); isReconcileClaim {
				// A backlog-reconcile claim's RunID (backlogreconcile.go) is a
				// synthesized lease identity, not a workflow run — it has no
				// rebase-pr/implement stages of its own for
				// preparePRRemediationNoopUpdate to find, and FindRunDir
				// rejects the id outright (it contains "/"). claimHolderTerminal
				// above already resolved terminality against the OWNING run;
				// that run's own claim (if any) still gets its own remediation
				// update prepared when this loop reaches it directly.
			} else if _, prepared := terminalUpdates[entry.RunID]; !prepared {
				update, err := preparePRRemediationNoopUpdate(l, entry.RunID)
				if err != nil {
					return nil, err
				}
				if update != nil {
					terminalUpdates[entry.RunID] = *update
				}
			}
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
		recorded := make(map[string]struct{})
		for _, entry := range terminalEntries {
			current, held := currentClaimEntry(ledger, entry)
			if !held || current.RunID != entry.RunID {
				continue
			}
			update, ok := terminalUpdates[entry.RunID]
			if !ok {
				continue
			}
			if _, ok := recorded[entry.RunID]; ok {
				continue
			}
			if err := recordPRRemediationNoopLocked(l, ledger, entry.RunID, update); err != nil {
				return err
			}
			recorded[entry.RunID] = struct{}{}
		}
		expired, err := ledger.RecoverExpired(now)
		if err != nil {
			return err
		}
		released = append(released, expired...)
		for _, entry := range terminalEntries {
			if interventionActive != nil && interventionActive(entry.RunID) {
				continue
			}
			current, held := currentClaimEntry(ledger, entry)
			if !held || current.RunID != entry.RunID {
				continue
			}
			terminal, err := claimHolderTerminal(l.Root, current)
			if err != nil {
				recordTerminalClaimInspectionError(log, current, err)
				continue
			}
			if !terminal {
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
