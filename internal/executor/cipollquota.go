package executor

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// waitForQuota leaves the ordinary failure counters untouched. A quota window
// is an advertised wait, not evidence that consecutive requests are broken.
func (e *CIPollExecutor) waitForQuota(ctx context.Context, w ciPollWait, cfg CIPollConfig, limit *providers.RateLimitError, failures int) (apiv1.ResultEnvelope, bool, error) {
	now := w.now()
	delay := limit.RetryDelay(now)
	jitter := time.Duration(rand.Int64N(int64(time.Second)))
	if e.Jitter != nil {
		jitter = max(0, min(time.Second, e.Jitter(time.Second)))
	}
	// Add slack without overflowing a maliciously large Retry-After.
	const slack = 2 * time.Second
	if delay < time.Duration(1<<63-1)-slack-jitter {
		delay += slack + jitter
	}
	deadline := w.deadline
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	remaining := max(time.Duration(0), deadline.Sub(now))
	wait := min(delay, remaining)
	if err := e.recordQuotaWait(ctx, journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Reason: "ci-poll waiting for provider quota",
		Runner: map[string]any{
			"kind": "ci_poll_rate_limit_wait", "provider": string(limit.Provider),
			"retryAt":          now.Add(delay).UTC().Format(time.RFC3339Nano),
			"waitMilliseconds": wait.Milliseconds(), "consecutiveErrors": failures,
			"secondary": limit.Secondary, "deadline": deadline.UTC().Format(time.RFC3339Nano),
		},
	}); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return quotaTimeout(w, cfg, limit), true, nil
		}
		return apiv1.ResultEnvelope{}, true, fmt.Errorf("journal ci-poll quota wait: %w", err)
	}
	// Keep the watchdog informed during a long, explicitly sanctioned wait.
	for wait > 0 {
		chunk := min(wait, 30*time.Second)
		if err := w.sleep(ctx, chunk); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return quotaTimeout(w, cfg, limit), true, nil
			}
			return apiv1.ResultEnvelope{}, true, err
		}
		invoke.ReportProgress(ctx)
		wait -= chunk
	}
	if delay >= remaining || !w.now().Before(deadline) {
		return quotaTimeout(w, cfg, limit), true, nil
	}
	return apiv1.ResultEnvelope{}, false, nil
}

func quotaTimeout(w ciPollWait, cfg CIPollConfig, limit *providers.RateLimitError) apiv1.ResultEnvelope {
	outcome := ciPollTimeoutOutcome(w.timeout, cfg.PullID)
	outcome.Summary = "ci-poll timed out waiting for provider quota"
	outcome.Error.Message = "ci-poll timeout before provider quota recovered"
	if !limit.Reset.IsZero() {
		reset := limit.Reset.UTC().Format(time.RFC3339)
		outcome.Outputs[OutputRateLimitReset] = reset
		outcome.Error.Message += "; advertised reset " + reset
	}
	return addCIPollCarry(outcome, cfg)
}

func (e *CIPollExecutor) recordQuotaWait(ctx context.Context, ev journal.Event) error {
	if sink, ok := e.Journal.(interface {
		AppendContext(context.Context, journal.Event) error
	}); ok {
		return sink.AppendContext(ctx, ev)
	}
	if sink, ok := e.Journal.(interface{ Append(journal.Event) error }); ok {
		return sink.Append(ev)
	}
	return nil
}
