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

// retryBaseDelay and retryMaxDelay bound the jittered exponential backoff
// between attempts. Vars, not consts, so a test can shrink them and observe
// several retries in bounded time — the same reason blobWriteThroughBudget
// (cmd/goobers/dispatchexec.go) is a var rather than a const.
var (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 30 * time.Second
)

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

// retryableStatus reports whether an HTTP response status is worth retrying:
// a 5xx is presumed transient (the endpoint or something in front of it is
// unhealthy); a 4xx is a permanent refusal a retry cannot fix (bad request,
// expired token, refused surrender).
func retryableStatus(statusCode int) bool {
	return statusCode >= 500
}
