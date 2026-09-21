package engine

import (
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
)

const retryBackoffObservationChange = "retry-backoff-observation-v1"

func sleepBeforeRetry(ctx workflow.Context, rec *runJournal, stage string, attempt int, class journal.AttemptClass, delay time.Duration) error {
	if workflow.GetVersion(ctx, retryBackoffObservationChange, workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		at := workflow.Now(ctx)
		rec.appendAt(at, journal.RetryBackoffEvent(stage, attempt, "engine", class, at, at.Add(delay)))
	}
	// Do not add a diagnostic-only activity or change retry timing/budgets.
	// Temporal history is authoritative; the existing emit/repair path may
	// deliver this annotation after the wait, leaving live observation unknown.
	return workflow.Sleep(ctx, delay)
}
