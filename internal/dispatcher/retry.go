package dispatcher

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// retry.go bounds the transient-failure exposure of this package's two
// daemon-facing PUT clients (SurrenderPutClient, BlobClient) — #4260. A brief
// control-plane restart (every rollout, since goobers-api is single-replica
// by construction, #3809) previously turned an already-finished stage
// attempt's single-shot PUT into a lost result on the very first refused or
// dropped connection. Both endpoints are idempotent upserts keyed by the
// caller's own identity (surrender: write-once by run/stage/attempt,
// surrender.go:298-302; blob: write-once by content digest, blob.go:111-113),
// so retrying the identical request is safe by construction — no client-side
// idempotency key is needed, only patience.

// defaultRetryBaseDelay and defaultRetryMaxDelay bound the production jittered
// exponential backoff between attempts.
const (
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 30 * time.Second
)

// RetryPolicy overrides retry pacing for one client. Zero values retain the
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

// retryBackoff returns a jittered duration between half and all of
// base<<attempt, capped at max — this package's own copy of the pattern
// internal/executor/cipoll.go's backoff uses (unexported there, so not
// importable).
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
//
// deadline bounds the WHOLE loop via ctx, so a caller-supplied ctx with its
// own earlier deadline (e.g. the 15s blob write-through batch budget,
// dispatchexec.go's blobWriteThroughBudget) wins automatically — this never
// widens a caller's existing bound, only fills in one where none exists.
func withRetry(ctx context.Context, deadline time.Duration, attempt func(ctx context.Context) (retryable bool, err error)) error {
	return withRetryPolicy(ctx, deadline, RetryPolicy{}, attempt)
}

func withRetryPolicy(ctx context.Context, deadline time.Duration, policy RetryPolicy, attempt func(ctx context.Context) (retryable bool, err error)) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	base, max := policy.delays()
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
		timer := time.NewTimer(retryBackoff(base, max, n))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retry deadline exceeded after %d attempt(s): %w", n+1, lastErr)
		case <-timer.C:
		}
	}
}

// retryableStatus reports whether an HTTP response status is worth retrying:
// a 5xx is presumed transient (the endpoint or something in front of it is
// unhealthy); a 4xx is a permanent refusal a retry cannot fix (bad request,
// expired token, refused surrender).
func retryableStatus(statusCode int) bool {
	return statusCode >= 500
}
