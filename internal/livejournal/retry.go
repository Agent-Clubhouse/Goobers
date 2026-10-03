package livejournal

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/retryutil"
)

// retry.go bounds HTTPEmitter.Emit's transient-failure exposure (#4260).
// Emit's redelivery is already safe: every op
// carries a caller-derived idempotency key the writer dedupes on
// (livejournal.go's Op.Key doc, applyOp), so retrying an identical batch
// after a dropped or refused connection can only be a no-op on the server
// side, never a double-apply.

// defaultRetryBaseDelay and defaultRetryMaxDelay bound the production jittered
// exponential backoff between attempts.
const (
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 30 * time.Second
)

// RetryPolicy overrides retry pacing for one emitter. Zero values retain the
// production defaults.
type RetryPolicy struct {
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

func (p RetryPolicy) delays() (time.Duration, time.Duration) {
	base, max := p.BaseDelay, p.MaxDelay
	if base <= 0 {
		base = defaultRetryBaseDelay
	}
	if max <= 0 {
		max = defaultRetryMaxDelay
	}
	if max < base {
		max = base
	}
	return base, max
}

// defaultEmitRetryDeadline bounds Emit's retry loop when the caller sets no
// RetryDeadline of its own. Short relative to the surrender plane's default:
// Emit rides hot paths (a heartbeat tick, a span append) that can run many
// times per stage, and a caller with a tighter context deadline already in
// force (e.g. the pod's 30s-per-heartbeat or 15s blob-write-through budgets)
// wins regardless, since context.WithTimeout always honors the earlier of
// the two.
const defaultEmitRetryDeadline = 60 * time.Second

// withRetryPolicy runs attempt until it succeeds (nil error), reports a
// non-retryable failure, or deadline elapses — whichever comes first —
// waiting a jittered backoff between tries. attempt classifies its own
// failure as retryable or not; withRetryPolicy owns only pacing and the
// deadline.
func withRetryPolicy(ctx context.Context, deadline time.Duration, policy RetryPolicy, attempt func(ctx context.Context) (retryable bool, err error)) error {
	base, max := policy.delays()
	return retryutil.Until(ctx, deadline, retryutil.Policy{Base: base, Max: max}, attempt)
}

// retryableStatus reports whether an HTTP response status is worth
// retrying: a 5xx is presumed transient; a 4xx (bad request, unauthenticated,
// unknown key format) is a permanent refusal a retry cannot fix.
func retryableStatus(statusCode int) bool {
	return statusCode >= 500
}
