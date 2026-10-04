package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func (r *Runner) yieldChildCustody(ctx context.Context, tf *taskFrame, attempt int, class journal.AttemptClass, request ChildHandoffRequest, custody ChildWorkspaceCustody) (string, error) {
	lastReason := ""
	for {
		err := r.cfg.ChildHandoff.Yield(ctx, request, custody)
		if err == nil {
			return "", nil
		}
		var blocked *ChildDispositionWaitError
		if !errors.As(err, &blocked) {
			return "", err
		}
		if len(blocked.Reason) > 1024 || blocked.Reason == "" {
			return "", fmt.Errorf("runner: invalid disposition refusal")
		}
		if blocked.Reason != lastReason {
			if err := tf.jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: tf.t.Name, Attempt: attempt, AttemptClass: class, Runner: map[string]any{"kind": "child.workflow.disposition-blocked", "requestId": request.RequestID, "reason": blocked.Reason, "reconciliationRequired": blocked.Reconcile}}); err != nil {
				return "", err
			}
			lastReason = blocked.Reason
		}
		if !blocked.Reconcile {
			return blocked.Reason, nil
		}
		// Repeated suspension retains the same owner and budget. No harness may
		// resume against a possibly partial application; retry only its exact plan.
		if tf.in.parallelChild == nil {
			if _, err := r.cfg.ChildParentCapacity.SuspendChildParent(ctx, tf.in.RunID); err != nil {
				return "", err
			}
		}
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}
