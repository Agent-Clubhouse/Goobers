package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/recovery"
)

type recoveryDeliveryService struct {
	layout instance.Layout
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
		phase, err := reader.PhaseBounded(ctx)
		if err != nil {
			return err
		}
		if !terminalRunPhase(phase) {
			return fmt.Errorf("recovery source is no longer terminal")
		}
		current, err := recovery.ReadRetainedRecord(selected.RecordPath)
		if err != nil {
			return err
		}
		if current != selected.Record || !time.Now().Before(current.RetainUntil) {
			return recovery.ErrRecordConflict
		}
		if _, err := authorizeRecoveryDelivery(ctx, s.layout, runID, repositoryKey, issueID, time.Now().UTC()); err != nil {
			return err
		}
		transferCtx, transferCancel := context.WithDeadline(ctx, current.RetainUntil)
		defer transferCancel()
		return recovery.WriteArchiveEnvelope(transferCtx, filepath.Join(filepath.Dir(selected.RecordPath), recovery.BundleFileName), current, 512<<20, out)
	})
	if err == nil && !entered {
		return fmt.Errorf("recovery source is busy")
	}
	return err
}

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
