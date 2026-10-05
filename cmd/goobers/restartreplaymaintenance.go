package main

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/restartintent"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (u *upSession) restartReplayMaintenance() func(context.Context) error {
	var cursor string
	return func(ctx context.Context) error {
		queue := u.durableTriggers.queue
		page, err := queue.HumanRestartMaintenance(ctx, cursor, 20)
		if err != nil {
			return err
		}
		service := &restartintent.Service{Queue: queue, Now: u.durableTriggers.startControls.Now}
		var failures error
		for _, candidate := range page {
			if err = ctx.Err(); err != nil {
				return errors.Join(failures, err)
			}
			if len(candidate.Replay) == 0 {
				control, readErr := queue.StartControl(ctx, candidate.Gaggle, candidate.Record.ID)
				if readErr != nil {
					failures = errors.Join(failures, readErr)
				} else if !service.Now().Before(control.DisposedAt.Add(triggerqueue.ReplayRetention)) {
					failures = errors.Join(failures, service.CompactRejected(ctx, candidate.Record))
				}
			}
			if candidate.Retiring && len(candidate.Replay) > 0 {
				failures = errors.Join(failures, u.forgetRetiredRestart(ctx, candidate))
			}
			cursor = candidate.Record.ID
		}
		if len(page) < 20 {
			cursor = ""
		}
		return failures
	}
}

func (u *upSession) forgetRetiredRestart(ctx context.Context, c triggerqueue.HumanRestartReplay) error {
	release, owned := u.setup.RunnerRegistry.acquireChildCustody(c.SourceRun)
	if !owned {
		return nil
	}
	defer release()
	roots, err := u.l.RunDirsContext(ctx)
	if err != nil {
		return err
	}
	// Include an absent scoped live root when only its staged sibling remains.
	roots = append(roots, u.l.ForGaggle(c.Gaggle).RunsDir(), u.l.RunsDir())
	locks, err := journal.TryAcquireRunRootMaintenanceLocks(roots)
	if errors.Is(err, platformlock.ErrHeld) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = locks.Release() }()
	path, err := acceptedTriggerJournalDir(ctx, u.l, c.SourceRun)
	if err != nil || path != "" {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return u.durableTriggers.queue.ForgetRetiredHumanRestart(ctx, c.Record.ID, c.Gaggle, c.SourceRun)
}

func markRestartSourcePruning(ctx context.Context, queue *triggerqueue.Store, candidate retention.Result, now time.Time) error {
	gaggle, marked, err := queue.HumanRestartSourceScope(ctx, candidate.RunID)
	if err != nil || gaggle == "" || marked {
		return err
	}
	reader, err := journal.OpenReadOnly(candidate.RunDir)
	if err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if id.RunID != candidate.RunID || id.Gaggle != gaggle {
		return triggerqueue.ErrConflict
	}
	// The enclosing telemetry prune guard has already reserved the terminal
	// journal and checked child/session/event/restart dependencies.
	return queue.MarkHumanRestartSourceRetiring(ctx, gaggle, id.RunID, now)
}
