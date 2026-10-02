package main

import (
	"errors"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

type schedulerClaimState struct{ recovered []localscheduler.ClaimEntry }
type schedulerClaimInput struct {
	Layout       instance.Layout
	RecoveryGate *localscheduler.RecoveryGate
	InstanceLog  *journal.InstanceLog
	Providers    map[string]apiv1.Provider
}

// openSchedulerClaimState owns startup claim recovery under the migration lock.
// withClaimLock releases that lock on every return, including ledger-open and
// migration failures. The ledger uses atomic file writes and holds no descriptor
// or background worker; the returned snapshot needs no Close. Durable migration
// and recovery changes intentionally survive a later startup failure.
func openSchedulerClaimState(input schedulerClaimInput) (*schedulerClaimState, error) {
	state := &schedulerClaimState{}

	err := withClaimLock(filepath.Join(input.Layout.SchedulerDir(), claimLockFileName), claimLockOperationMigration, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(input.Layout.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithInstanceLog(input.InstanceLog),
		)
		if err != nil {
			return err
		}
		// DS6 (distributed-state-and-coordination.md §10): a daemon start must
		// rebuild its renewal set from ledger + liveness BEFORE any reap runs,
		// so `goobers up` closes this gate and reaps in its own startup recovery
		// pass after the rebuild. One-shot callers do the same when `engine:` is
		// configured; only a pure mode-1 one-shot passes no gate and keeps
		// reaping here as before.
		if input.RecoveryGate.RecoveryPermitted() {
			state.recovered, err = ledger.RecoverExpired(time.Now())
			if err != nil {
				return err
			}
		}
		return ledger.MigrateLegacyClaims(func(entry localscheduler.ClaimEntry) (localscheduler.ClaimNamespace, error) {
			namespace, resolveErr := legacyClaimNamespace(input.Layout, input.Providers, entry)
			if errors.Is(resolveErr, localscheduler.ErrLegacyClaimOwnershipUnresolved) {
				input.InstanceLog.AppendBestEffort(journal.Event{
					Type: journal.EventError, RunID: entry.RunID, Workflow: entry.Workflow,
					Error: journal.ErrorDetailFor("legacy_claim_ownership_unresolved", resolveErr),
				})
			}
			return namespace, resolveErr
		})
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}
