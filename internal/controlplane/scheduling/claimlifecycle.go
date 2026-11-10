package scheduling

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// ReleaseForRun releases a terminal holder using the supplied lock discipline.
func (c ClaimLifecycle) ReleaseForRun(l instance.Layout, log *journal.InstanceLog, runID string, lock func(string, func() error) error) error {
	ledger, err := claimsclient.NewFile(claimsclient.FileConfig{LedgerPath: filepath.Join(l.SchedulerDir(), ClaimLedgerFileName),
		Lock: lock, ProviderReleaseLock: c.Files.ProviderReleaseLock(l), Shared: c.SharedResolver(l), Options: WithClaimJournal(log)})
	if err != nil {
		return fmt.Errorf("open claim ledger: %w", err)
	}
	if held, err := ledger.ForRunAll(ClaimContext(), runID); err != nil || len(held) == 0 {
		return err
	}
	return claimsclient.Sequence(ClaimContext(), ledger, ClaimLockOperationRunRelease, func(tx claimsclient.Ledger) error {
		_, err := tx.ReleaseAllForRun(ClaimContext(), runID, c.HolderReleaseReason(l, runID))
		return err
	})
}

// HolderReleaseReason uses the terminal outcome or reconcile ownership as the release reason.
//
// claimHolderReleaseReason is why a terminal holder's claims end: the run's
// own terminal outcome, or reconciliation for a backlog-reconcile reservation
// whose owning run ended.
func (c ClaimLifecycle) HolderReleaseReason(l instance.Layout, runID string) claimrelease.Reason {
	if c.IsReconcile(runID) {
		return claimrelease.Reconciled.Because("reconcile_owner_terminal", "")
	}
	return localscheduler.TerminalReleaseReason(l.FindRunDir, runID)
}

// RenewLive renews ledger entries whose holders are reported live.
//
// renewLiveClaims re-acquires every LEDGER claim whose holding run is live
// (DS6, docs/design/distributed-state-and-coordination.md §6/§10). The ledger
// itself — not this process's in-memory run tracking — is the source of truth
// for WHICH claims are renewal candidates; probe decides WHETHER each holder
// is live: tracked in-process (issue #2014's signal, still valid — chosen
// over a per-stage heartbeat because a stage runs as a subprocess with no
// reach into the daemon process's claim ledger) or an open engine workflow
// when `engine:` is configured, which is what a daemon restart cannot lose.
//
// Probing happens OUTSIDE the claims lock — an engine describe can block on
// network, and holding the ledger lock across that would starve every
// claimant — so only entries re-read from the ledger under the lock are
// renewed (ClaimLedger.RenewRuns): a claim released between snapshot and
// renewal is never resurrected.
//
// probeErr carries fail-live probe degradations (those runs WERE renewed);
// err carries ledger/lock failures (renewal did not durably complete — a
// caller holding a RecoveryGate must not mark it rebuilt on non-nil err).
//
// Opens the ledger WITHOUT WithInstanceLog deliberately: RenewEntry reuses
// Claim/ClaimScoped, which journals claim.acquired on every success, and a
// routine renewal every claimRecoverInterval for every live run is
// bookkeeping, not an audit-worthy event — journaling it here would add a
// recurring InstanceLog.Append (a real cost per its own doc) purely to
// restate what the original claim and any eventual release already record.
func (c ClaimLifecycle) RenewLive(ctx context.Context, l instance.Layout, probe localscheduler.RunLivenessProbe, leaseDuration time.Duration) (renewed []localscheduler.ClaimEntry, probeErr, err error) {
	ledgerPath := filepath.Join(l.SchedulerDir(), ClaimLedgerFileName)
	snapshot, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open claim ledger: %w", err)
	}
	entries := snapshot.Snapshot()
	if len(entries) == 0 {
		return nil, nil, nil
	}
	live, probeErr := localscheduler.ProbeLiveClaimHolders(ctx, entries, probe)
	if len(live) == 0 {
		return nil, probeErr, nil
	}
	renewed, err = c.RenewCoordinated(ctx, l, entries, live, leaseDuration, c.SharedResolver(l))
	return renewed, probeErr, err
}

// Recover releases expired and terminal claims after the recovery gate opens.
//
// recoverClaims releases expired leases and leases whose owning run is already
// terminal. The latter retries claim cleanup deferred by a claims-lock timeout.
//
// gate enforces DS6's load-bearing ordering
// (docs/design/distributed-state-and-coordination.md §10): on daemon start the
// renewal set is rebuilt from ledger + liveness BEFORE this reap is permitted
// to run — while the gate is closed the pass is a no-op, so a freshly
// restarted daemon cannot reap a live distributed run's claims in the gap
// before renewals flow again. Nil gate on one-shot `goobers run`/`signal`
// paths permits recovery, preserving their local behavior.
func (c ClaimLifecycle) Recover(
	l instance.Layout,
	log *journal.InstanceLog,
	now time.Time,
	interventionActive func(string) bool,
	gate *localscheduler.RecoveryGate,
) ([]localscheduler.ClaimEntry, error) {
	return c.RecoverWithResolver(l, log, now, interventionActive, gate, c.SharedResolver(l))
}

// RecoverWithResolver recovers claims with the specified shared provider coordination.
func (c ClaimLifecycle) RecoverWithResolver(
	l instance.Layout,
	log *journal.InstanceLog,
	now time.Time,
	interventionActive func(string) bool,
	gate *localscheduler.RecoveryGate,
	resolver claimsclient.SharedClaimResolver,
) ([]localscheduler.ClaimEntry, error) {
	if !gate.RecoveryPermitted() {
		return nil, nil
	}
	ledgerPath := filepath.Join(l.SchedulerDir(), ClaimLedgerFileName)
	snapshot, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		return nil, err
	}
	var terminalEntries []localscheduler.ClaimEntry
	terminalUpdates := make(map[string]RemediationUpdate)
	// Release reasons are read once per terminal run, outside the claims lock.
	reasons := make(map[string]claimrelease.Reason)
	for _, entry := range snapshot.Snapshot() {
		if entry.IsPublicationClaim() {
			continue
		}
		terminal, err := c.HolderTerminal(l.Root, entry)
		if err != nil {
			recordTerminalClaimInspectionError(log, entry, err)
			continue
		}
		if terminal {
			terminalEntries = append(terminalEntries, entry)
			if _, known := reasons[entry.RunID]; !known {
				reasons[entry.RunID] = c.HolderReleaseReason(l, entry.RunID)
			}
			if c.IsReconcile(entry.RunID) {
				// A backlog-reconcile claim's RunID (backlogreconcile.go) is a
				// synthesized lease identity, not a workflow run — it has no
				// rebase-pr/implement stages of its own for
				// preparePRRemediationNoopUpdate to find, and FindRunDir
				// rejects the id outright (it contains "/"). claimHolderTerminal
				// above already resolved terminality against the OWNING run;
				// that run's own claim (if any) still gets its own remediation
				// update prepared when this loop reaches it directly.
			} else if _, prepared := terminalUpdates[entry.RunID]; !prepared {
				update, err := c.PrepareRemediation(l, entry.RunID)
				if err != nil {
					return nil, err
				}
				if update != nil {
					terminalUpdates[entry.RunID] = update
				}
			}
		}
	}

	var released []localscheduler.ClaimEntry
	var sharedExpired, sharedTerminal []localscheduler.ClaimEntry
	err = c.Files.ClaimLock(filepath.Join(l.SchedulerDir(), ClaimLockFileName), ClaimLockOperationRecovery, func() error {
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
			if ok {
				if _, alreadyRecorded := recorded[entry.RunID]; alreadyRecorded {
					continue
				}
				if err := update(context.Background(), ledger.ForRunAll(entry.RunID)); err != nil {
					return err
				}
				recorded[entry.RunID] = struct{}{}
			}
		}
		var cleanupErr error
		for _, entry := range ledger.Snapshot() {
			if !entry.SharedDeadline.IsZero() && !entry.ExpiresAt.After(now) {
				sharedExpired = append(sharedExpired, entry)
			}
		}
		expired, err := ledger.RecoverExpired(now)
		if err != nil {
			return errors.Join(cleanupErr, err)
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
			terminal, err := c.HolderTerminal(l.Root, current)
			if err != nil {
				recordTerminalClaimInspectionError(log, current, err)
				continue
			}
			if !terminal {
				continue
			}
			if !current.SharedDeadline.IsZero() {
				sharedTerminal = append(sharedTerminal, current)
				continue
			}
			if err := ledger.ReleaseEntry(current, current.RunID, reasons[current.RunID]); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release terminal claim %s for run %s: %w", entry.ItemID, entry.RunID, err))
				continue
			}
			released = append(released, current)
		}
		return cleanupErr
	})
	if err != nil {
		return released, err
	}
	client, err := c.CoordinatedLedger(l, log, resolver)
	if err != nil {
		return released, err
	}
	expired, cleanupErr := RecoverExpiredSharedClaims(ClaimContext(), sharedExpired, client, now)
	released = append(released, expired...)
	for _, entry := range sharedTerminal {
		if !entry.ExpiresAt.After(now) {
			continue
		} // already attempted above
		if didRelease, err := client.ReleaseExpected(ClaimContext(), entry, reasons[entry.RunID]); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release terminal claim %s for run %s: %w", entry.ItemID, entry.RunID, err))
		} else if didRelease {
			released = append(released, entry)
		}
	}
	return released, cleanupErr
}

// RecoverExpiredSharedClaims revalidates expired shared leases under short locks.
//
// Each release revalidates the snapshot under a short lock. Remote cleanup
// cannot prevent an unrelated local claim or the next lifecycle read.
func RecoverExpiredSharedClaims(ctx context.Context, entries []localscheduler.ClaimEntry, coordinated *claimsclient.File, now time.Time) ([]localscheduler.ClaimEntry, error) {
	var released []localscheduler.ClaimEntry
	var cleanupErr error
	for _, entry := range entries {
		if entry.SharedDeadline.IsZero() || entry.ExpiresAt.After(now) {
			continue
		}
		if didRelease, err := coordinated.ReleaseExpiredExpected(ctx, entry, localscheduler.StallSweptReason(), now); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release expired shared claim %s for run %s: %w", entry.ItemID, entry.RunID, err))
		} else if didRelease {
			released = append(released, entry)
		}
	}
	return released, cleanupErr
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
	log.AppendBestEffort(journal.Event{
		Type:     journal.EventError,
		Name:     entry.ItemID,
		Gaggle:   entry.Gaggle,
		Workflow: entry.Workflow,
		RunID:    entry.RunID,
		Error:    journal.ErrorDetailFor("terminal_claim_inspection_failed", err),
		Runner:   map[string]any{"operation": ClaimLockOperationRecovery},
	})
}

// RemediationUpdate is prepared before taking the claims lock and applied to
// the current holder entries inside that same lock.
type RemediationUpdate func(context.Context, []localscheduler.ClaimEntry) error

// ClaimLifecycle injects provider coordination and command-owned lock policy.
// These lifecycle paths explicitly OpenClaimLedger and propagate open errors;
// their concrete *ClaimLedger snapshots are infallible in-memory reads. Cloud
// claim maintenance uses claims.Plane, not these filesystem callbacks (#8146).
type ClaimLifecycle struct {
	Files              ClaimFiles
	SharedResolver     func(instance.Layout) claimsclient.SharedClaimResolver
	IsReconcile        func(string) bool
	HolderTerminal     func(string, localscheduler.ClaimEntry) (bool, error)
	PrepareRemediation func(instance.Layout, string) (RemediationUpdate, error)
	CoordinatedLedger  func(instance.Layout, *journal.InstanceLog, claimsclient.SharedClaimResolver) (*claimsclient.File, error)
	RenewCoordinated   func(context.Context, instance.Layout, []localscheduler.ClaimEntry, map[string]bool, time.Duration, claimsclient.SharedClaimResolver) ([]localscheduler.ClaimEntry, error)
}
