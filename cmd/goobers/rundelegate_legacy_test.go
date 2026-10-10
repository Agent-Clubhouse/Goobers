package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Legacy protocol regression tests exercise the original direct-dispatch seam.
// Production always supplies durable admission through the daemon sweep.
func sweepPendingTriggers(ctx context.Context, schedulerDir string, log *journal.InstanceLog, sched *localscheduler.Scheduler, now func() time.Time) error {
	return sweepPendingTriggersWithOptions(ctx, schedulerDir, log, sched, now, triggerSweepOptions{})
}

func sweepPendingTriggersWithOptions(ctx context.Context, schedulerDir string, log *journal.InstanceLog, sched *localscheduler.Scheduler, now func() time.Time, options triggerSweepOptions) error {
	return sweepPendingTriggersWithAdmission(ctx, schedulerDir, log, sched, now, options, nil)
}
