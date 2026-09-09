package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
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

// releaseClaimsForRunLocked runs inside the caller's held claims lock, so it
// takes the lock-free file ledger (heldClaimLedger) — the daemon is the
// writer here, never a plane client.
func releaseClaimsForRunLocked(l instance.Layout, log *journal.InstanceLog, runID string) error {
	ledger, err := heldClaimLedger(l, localscheduler.WithInstanceLog(log))
	if err != nil {
		return fmt.Errorf("open claim ledger: %w", err)
	}
	_, err = ledger.ReleaseAllForRun(claimContext(), runID)
	return err
}

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
func renewLiveClaims(ctx context.Context, l instance.Layout, probe localscheduler.RunLivenessProbe, leaseDuration time.Duration) (renewed []localscheduler.ClaimEntry, probeErr, err error) {
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
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
	err = withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationRenewal, func() error {
		ledger, openErr := localscheduler.OpenClaimLedger(ledgerPath)
		if openErr != nil {
			return fmt.Errorf("open claim ledger: %w", openErr)
		}
		var renewErr error
		renewed, renewErr = renewCoordinatedLiveClaims(ctx, l, ledger, live, leaseDuration, localLifecycleSharedClaimResolver(l))
		return renewErr
	})
	return renewed, probeErr, err
}

// recoverClaims releases expired leases and leases whose owning run is already
// terminal. The latter retries claim cleanup deferred by a claims-lock timeout.
//
// gate enforces DS6's load-bearing ordering
// (docs/design/distributed-state-and-coordination.md §10): on daemon start the
// renewal set is rebuilt from ledger + liveness BEFORE this reap is permitted
// to run — while the gate is closed the pass is a no-op, so a freshly
// restarted daemon cannot reap a live distributed run's claims in the gap
// before renewals flow again. Nil gate (the one-shot `goobers run`/`signal`
// paths on a pure mode-1 instance) permits, preserving their existing
// behavior; an engine-configured one-shot passes a real gate through
// oneShotClaimRecovery.
func recoverClaims(
	l instance.Layout,
	log *journal.InstanceLog,
	now time.Time,
	interventionActive func(string) bool,
	gate *localscheduler.RecoveryGate,
) ([]localscheduler.ClaimEntry, error) {
	return recoverClaimsWithResolver(l, log, now, interventionActive, gate, localLifecycleSharedClaimResolver(l))
}

func recoverClaimsWithResolver(
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
		service := &daemonClaimService{layout: l, shared: resolver}
		coordinated, err := service.coordinatedLedger(ledger)
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
				if err := recordPRRemediationNoopLocked(
					context.Background(), l, ledger.ForRunAll(entry.RunID), entry.RunID, update); err != nil {
					return err
				}
				recorded[entry.RunID] = struct{}{}
			}
		}
		sharedExpired, cleanupErr := recoverExpiredSharedClaims(claimContext(), ledger, coordinated, now)
		released = append(released, sharedExpired...)
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
			terminal, err := claimHolderTerminal(l.Root, current)
			if err != nil {
				recordTerminalClaimInspectionError(log, current, err)
				continue
			}
			if !terminal {
				continue
			}
			if err := coordinated.ReleaseScoped(claimContext(), claimsclient.KeyForEntry(current), current.RunID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release terminal claim %s for run %s: %w", entry.ItemID, entry.RunID, err))
				continue
			}
			released = append(released, current)
		}
		return cleanupErr
	})
	return released, err
}

// Called under the same cross-process lock as the terminal recovery pass.
// Retain failed cleanup in the ledger so a later pass can retry with the
// exact persisted owner; an expired lease is never renewal authority.
func recoverExpiredSharedClaims(ctx context.Context, ledger *localscheduler.ClaimLedger, coordinated claimsclient.Ledger, now time.Time) ([]localscheduler.ClaimEntry, error) {
	var released []localscheduler.ClaimEntry
	var cleanupErr error
	for _, entry := range ledger.Snapshot() {
		if entry.SharedDeadline.IsZero() || entry.ExpiresAt.After(now) {
			continue
		}
		if err := coordinated.ReleaseScoped(ctx, claimsclient.KeyForEntry(entry), entry.RunID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release expired shared claim %s for run %s: %w", entry.ItemID, entry.RunID, err))
			continue
		}
		released = append(released, entry)
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
