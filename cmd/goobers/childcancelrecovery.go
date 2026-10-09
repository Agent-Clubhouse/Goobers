package main

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// finishCancelledChild runs only inside Result's exclusive registry custody,
// after the physical-worker reconciler has joined all dispatched writers.
// Re-read accepted lineage and cancellation under the journal's exclusive lock;
// a stale queue observation never licenses either restart or terminalization.
func (l *queuedChildLauncher) finishCancelledChild(ctx context.Context, reader *journal.Reader) (bool, error) {
	id, err := reader.Identity()
	if err != nil {
		return false, err
	}
	ref, err := retainedChildExecutionRef(ctx, l.queue, id, false)
	if err != nil {
		return false, err
	}
	if !ref.Child.CancellationRequested {
		return false, nil
	}
	writer, _, err := journal.TryRecover(reader.Dir())
	if err != nil {
		return false, err
	}
	defer func() { _ = writer.Close() }()
	current, err := retainedChildExecutionRef(ctx, l.queue, id, false)
	if err != nil || !current.Child.CancellationRequested {
		return false, errors.Join(errors.New("child cancellation custody changed"), err)
	}
	result, err := runner.FinalizeCancelledChild(reader, writer, id, time.Now())
	if err != nil {
		return false, err
	}
	if result.Phase != journal.PhaseRunning && l.dispatch != nil {
		if scheduler := l.dispatch.sched.Load(); scheduler != nil {
			scheduler.ReleaseReconciled(id.RunID, id.Workflow)
		}
	}
	return true, nil
}
