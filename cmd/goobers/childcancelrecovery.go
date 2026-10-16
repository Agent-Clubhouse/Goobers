package main

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/invoke"
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

// A worker can observe the durable family fence before the queue delivers
// CancelRun to its host owner. Its returned authority error is then a stopped
// invocation, not a new failed child outcome. Leave terminalization to the
// existing exclusive queue owner, which verifies physical custody again.
func (p *childStagePod) deferCancelledOutcome(ctx context.Context) error {
	owned, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ref, err := retainedChildExecutionRef(owned, p.service.childQueue, p.identity, false)
	if err != nil {
		return errors.Join(invoke.ErrChildCustodyPending, err)
	}
	if ref.Child.CancellationRequested {
		return invoke.ErrChildCustodyPending
	}
	return nil
}
