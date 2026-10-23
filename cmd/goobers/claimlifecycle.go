package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func claimLifecycle() scheduling.ClaimLifecycle {
	return scheduling.ClaimLifecycle{
		Files: claimFiles(), SharedResolver: localLifecycleSharedClaimResolver,
		IsReconcile:    func(id string) bool { _, ok := parseBacklogReconcileRunID(id); return ok },
		HolderTerminal: claimHolderTerminal, RenewCoordinated: renewCoordinatedLiveClaims,
		PrepareRemediation: func(l instance.Layout, id string) (scheduling.RemediationUpdate, error) {
			update, err := preparePRRemediationNoopUpdate(l, id)
			if err != nil || update == nil {
				return nil, err
			}
			return func(ctx context.Context, entries []localscheduler.ClaimEntry) error {
				return recordPRRemediationNoopLocked(ctx, l, entries, id, *update)
			}, nil
		},
		CoordinatedLedger: func(l instance.Layout, log *journal.InstanceLog, resolver claimsclient.SharedClaimResolver) (*claimsclient.File, error) {
			return newBareDaemonClaimService(l, log, resolver).coordinatedLedger(claimLockOperationRecovery, httpapi.ClaimRequest{})
		},
	}
}

// releaseClaimsForRun releases work claims owned by runID, preserving fixed
// publication custody until verified release or expiry. It is safe to call
// after an explicit workflow release already did the same work.
func releaseClaimsForRun(l instance.Layout, log *journal.InstanceLog, runID string) error {
	return releaseClaimsForRunUsing(l, log, runID, func(operation string, fn func() error) error {
		return withClaimLockForRun(filepath.Join(l.SchedulerDir(), claimLockFileName), operation, l.Gaggle(), runID, fn)
	})
}

func releaseClaimsForRunWithDefaultTimeout(l instance.Layout, log *journal.InstanceLog, runID string) error {
	return releaseClaimsForRunUsing(l, log, runID, func(operation string, fn func() error) error {
		return withClaimLockBounds(filepath.Join(l.SchedulerDir(), claimLockFileName), operation, instance.DefaultClaimsLockTimeout, claimLockSlowThreshold, claimLockEventContext{Gaggle: l.Gaggle(), RunID: runID}, fn)
	})
}

func releaseClaimsForRunUsing(l instance.Layout, log *journal.InstanceLog, runID string, lock func(string, func() error) error) error {
	return claimLifecycle().ReleaseForRun(l, log, runID, lock)
}

func claimHolderReleaseReason(l instance.Layout, runID string) claimrelease.Reason {
	return claimLifecycle().HolderReleaseReason(l, runID)
}

func renewLiveClaims(ctx context.Context, l instance.Layout, probe localscheduler.RunLivenessProbe, leaseDuration time.Duration) (renewed []localscheduler.ClaimEntry, probeErr, err error) {
	return claimLifecycle().RenewLive(ctx, l, probe, leaseDuration)
}

func recoverClaims(
	l instance.Layout,
	log *journal.InstanceLog,
	now time.Time,
	interventionActive func(string) bool,
	gate *localscheduler.RecoveryGate,
) ([]localscheduler.ClaimEntry, error) {
	return claimLifecycle().Recover(l, log, now, interventionActive, gate)
}

var recoverExpiredSharedClaims = scheduling.RecoverExpiredSharedClaims
