package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func waitForRetry(ctx, attemptCtx context.Context, jr journalAppender, stage string, attempt int, class journal.AttemptClass, delay time.Duration) error {
	if ctx.Err() != nil || attemptCtx.Err() != nil {
		return nil
	}
	observedAt := time.Now().UTC()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	if err := jr.Append(journal.RetryBackoffEvent(stage, attempt, "local", class, observedAt, observedAt.Add(delay))); err != nil {
		return fmt.Errorf("runner: journal retry backoff for %q: %w", stage, err)
	}
	// Preserve the original delay and cancellation behavior. Journal work runs
	// while this already-scheduled timer advances; no inferred remaining delay.
	select {
	case <-timer.C:
	case <-ctx.Done():
	case <-attemptCtx.Done():
	}
	return nil
}

func resetRetryBackoffOnResume(jr *journal.Run, events []journal.Event) error {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != journal.EventRunnerAnnotation {
			continue
		}
		switch events[i].Runner["kind"] {
		case journal.RetryBackoffResetKind:
			return nil
		case journal.RetryBackoffKind:
			return jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": journal.RetryBackoffResetKind}})
		}
	}
	return nil
}
