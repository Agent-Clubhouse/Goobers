package livejournal

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// retry.go bounds HTTPEmitter.Emit's transient-failure exposure (#4260),
// mirroring internal/dispatcher's own copy of this pattern (unexported
// there, so not importable — this package stays beneath the API layers, per
// client.go's package doc). Emit's redelivery is already safe: every op
// carries a caller-derived idempotency key the writer dedupes on
// (livejournal.go's Op.Key doc, applyOp), so retrying an identical batch
// after a dropped or refused connection can only be a no-op on the server
// side, never a double-apply.

// retryBaseDelay and retryMaxDelay bound the jittered exponential backoff
// between attempts. Vars, not consts, so a test can shrink them and observe
// several retries in bounded time — mirroring internal/dispatcher's copy.
var (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 30 * time.Second
)

// defaultEmitRetryDeadline bounds Emit's retry loop when the caller sets no
// RetryDeadline of its own. Short relative to the surrender plane's default:
// Emit rides hot paths (a heartbeat tick, a span append) that can run many
// times per stage, and a caller with a tighter context deadline already in
// force (e.g. the pod's 30s-per-heartbeat or 15s blob-write-through budgets)
// wins regardless, since context.WithTimeout always honors the earlier of
// the two.
const defaultEmitRetryDeadline = 60 * time.Second

// retryBackoff returns a jittered duration between half and all of
// base<<attempt, capped at max.
func retryBackoff(base, max time.Duration, attempt int) time.Duration {
	ceiling := base << attempt
	if ceiling <= 0 || ceiling > max {
		ceiling = max
	}
	floor := ceiling / 2
	return floor + time.Duration(rand.Int64N(int64(ceiling-floor)+1))
}

// withRetry runs attempt until it succeeds (nil error), reports a
// non-retryable failure, or deadline elapses — whichever comes first —
// waiting a jittered backoff between tries. attempt classifies its OWN
// failure as retryable or not; withRetry owns only pacing and the deadline.
func withRetry(ctx context.Context, deadline time.Duration, attempt func(ctx context.Context) (retryable bool, err error)) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var lastErr error
	for n := 0; ; n++ {
		retryable, err := attempt(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
		timer := time.NewTimer(retryBackoff(retryBaseDelay, retryMaxDelay, n))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retry deadline exceeded after %d attempt(s): %w", n+1, lastErr)
		case <-timer.C:
		}
	}
}

// retryableStatus reports whether an HTTP response status is worth
// retrying: a 5xx is presumed transient; a 4xx (bad request, unauthenticated,
// unknown key format) is a permanent refusal a retry cannot fix.
func retryableStatus(statusCode int) bool {
	return statusCode >= 500
}
