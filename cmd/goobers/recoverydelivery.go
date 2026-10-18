package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/sharedclaim"
)

type recoveryDeliveryService struct {
	layout instance.Layout
	setup  *schedulerSetup
}

func (s recoveryDeliveryService) StreamRecovery(ctx context.Context, runID, repositoryKey, issueID string, out io.Writer) error {
	deadline, err := authorizeRecoveryDelivery(ctx, s.layout, runID, repositoryKey, issueID, time.Now().UTC())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	selected, err := selectIssueRecovery(ctx, s.layout, repositoryKey, issueID, time.Now().UTC())
	if err != nil {
		return err
	}
	if selected.Record.RunID == runID {
		return fmt.Errorf("recovery delivery requires a distinct source run")
	}
	dir, err := runDirFor(s.layout, selected.Record.RunID)
	if err != nil {
		return err
	}
	entered, err := journal.WithIdleRunReader(ctx, dir, func(reader *journal.Reader) error {
		identity, err := reader.Identity()
		if err != nil || identity.RunID != selected.Record.RunID {
			return fmt.Errorf("recovery source identity changed")
		}
		sourceRun, err := recovery.ReadTerminalSourceRun(reader, identity)
		if err != nil {
			return err
		}
		current, err := validateRecoverySelection(s.layout, selected, time.Now().UTC())
		if err != nil {
			return err
		}
		if _, err := authorizeRecoveryDelivery(ctx, s.layout, runID, repositoryKey, issueID, time.Now().UTC()); err != nil {
			return err
		}
		if current.ArchiveBytes == 0 {
			return s.overflowPending(ctx)
		}
		transferCtx, transferCancel := context.WithDeadline(ctx, current.RetainUntil)
		defer transferCancel()
		return recovery.WriteArchiveEnvelope(transferCtx, filepath.Join(filepath.Dir(selected.RecordPath), recovery.BundleFileName), current, 512<<20, recovery.ReportingSourceRun(out, sourceRun))
	})
	if err == nil && !entered {
		return fmt.Errorf("recovery source is busy")
	}
	return err
}

func (s recoveryDeliveryService) overflowPending(ctx context.Context) error {
	cfg, err := instance.LoadConfig(s.layout.ConfigFile())
	if err != nil {
		return err
	}
	policy, _ := resolveRecoveryPolicy(s.layout, cfg)
	free, err := recoveryInventoryFreeSlots(ctx, s.layout, policy)
	if err != nil {
		return err
	}
	// Unlike the inventory cap, the sweep's enabled/dry-run settings are
	// startup configuration, so report the running daemon's values.
	if s.setup != nil && s.setup.Config != nil {
		cfg = s.setup.Config
	}
	return recovery.PendingPromotion(cfg.Retention.EnabledEffective(), cfg.Retention.DryRun, free)
}

// authorizeRecoveryDelivery checks the current lease and durable issue identity
// for an authenticated receiving run. runID must come from authentication, not
// an untrusted request field. The returned deadline bounds the transfer context;
// this does not authorize arbitrary cross-run reads or bypass archive selection.
func authorizeRecoveryDelivery(ctx context.Context, layout instance.Layout, runID, repositoryKey, issueID string, now time.Time) (time.Time, error) {
	return withAuthorizedRecoveryDelivery(ctx, layout, runID, repositoryKey, issueID, now, false, nil)
}

// withAuthorizedRecoveryDelivery holds the claim lock through an optional short
// durable acknowledgement. Callers must never transfer archive bytes or acquire
// repository locks in acknowledge: claim release must not wait for a transfer.
// Only publication may finish under a released local lease; receiving another
// run's archive always requires current ownership.
func withAuthorizedRecoveryDelivery(ctx context.Context, layout instance.Layout, runID, repositoryKey, issueID string, now time.Time, allowReleased bool, acknowledge func() error) (time.Time, error) {
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
		// Waiting for the lock must not extend a lease that expired meanwhile.
		if current := time.Now().UTC(); current.After(now) {
			now = current
		}
		claim, ok := recoveryDeliveryClaim(ledger, runID, issueID, now, allowReleased)
		if !ok {
			return fmt.Errorf("recovery delivery requires exclusive unexpired issue authorization")
		}
		deadline = claim.ExpiresAt
		if acknowledge != nil {
			return acknowledge()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return deadline, nil
}

// recoveryDeliveryClaim admits late publication under a normally terminated
// local lease only inside its original expiry window and without a successor.
// The claim lock held by the caller makes that fence continuous through a
// durable acknowledgement. Released shared leases cannot use this fallback:
// their provider may already have admitted a successor on another instance.
func recoveryDeliveryClaim(ledger *localscheduler.ClaimLedger, runID, issueID string, now time.Time, allowReleased bool) (localscheduler.ClaimEntry, bool) {
	claims := ledger.ForRunAll(runID)
	if len(claims) == 1 && claims[0].ItemID == issueID && claims[0].ReleasedAt == nil && claims[0].ExpiresAt.After(now) {
		return claims[0], true
	}
	if !allowReleased || len(claims) != 0 {
		return localscheduler.ClaimEntry{}, false
	}
	for _, current := range ledger.Snapshot() {
		if (current.ItemID == issueID || current.ExternalID == issueID) && current.ExpiresAt.After(now) {
			return localscheduler.ClaimEntry{}, false
		}
	}
	var matched []localscheduler.ClaimEntry
	for _, prior := range ledger.HistoryForRun(runID) {
		if !prior.SharedDeadline.IsZero() || prior.SharedOwner != (sharedclaim.Owner{}) || prior.SharedRevoked {
			continue
		}
		switch prior.ReleaseReason.Disposition {
		case claimrelease.Completed, claimrelease.Failed, claimrelease.Escalated:
		default:
			continue
		}
		if (prior.ItemID == issueID || prior.ExternalID == issueID) && prior.ReleasedAt != nil && prior.ExpiresAt.After(now) {
			matched = append(matched, prior)
		}
	}
	if len(matched) != 1 {
		return localscheduler.ClaimEntry{}, false
	}
	return matched[0], true
}
