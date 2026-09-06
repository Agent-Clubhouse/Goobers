package githubapp

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"
)

// retry.go bounds a transient failure from GitHub's installation-token mint
// endpoint (#3792): a brief upstream 500 or rate-limit response previously
// went straight to mintError indistinguishable from a permanent
// misconfiguration, and the runner's own outer retry completes both attempts
// within a few seconds — too tight a window for GitHub's blip to clear. This
// package's own copy of the jittered-backoff pattern (internal/dispatcher's
// retry.go and internal/executor/cipoll.go's backoff each keep their own,
// unexported, rather than sharing one) so a mint's retry policy stays local
// to what it retries.

// mintRetryBaseDelay and mintRetryMaxDelay bound the jittered exponential
// backoff between mint attempts. Vars, not consts, so a test can shrink them
// and observe several retries in bounded time.
var (
	mintRetryBaseDelay = 200 * time.Millisecond
	mintRetryMaxDelay  = 2 * time.Second
)

// mintMaxAttempts caps the retry loop independent of the deadline: even a
// generous mintTimeout must not turn into dozens of attempts against an
// endpoint that is simply down.
const mintMaxAttempts = 4

// mintBackoff returns a jittered duration between half and all of
// base<<attempt, capped at max.
func mintBackoff(base, max time.Duration, attempt int) time.Duration {
	ceiling := base << attempt
	if ceiling <= 0 || ceiling > max {
		ceiling = max
	}
	floor := ceiling / 2
	return floor + time.Duration(rand.Int64N(int64(ceiling-floor)+1))
}

// mintRetryableStatus reports whether a mint HTTP response status is worth
// retrying: a 5xx is presumed transient (GitHub or something in front of it
// is unhealthy) and 429 is a rate limit that clears with time; every other
// 4xx — 401, 403, 404 included — is a refusal a retry cannot fix.
func mintRetryableStatus(statusCode int) bool {
	return statusCode >= 500 || statusCode == http.StatusTooManyRequests
}

// mintWithRetry runs attempt until it succeeds (nil error), reports a
// non-retryable failure, the attempt budget is spent, or ctx's deadline
// elapses — whichever comes first — waiting a jittered backoff between
// tries. attempt classifies its own failure as retryable or not; this loop
// owns only pacing, the attempt cap, and the deadline. Returns the last
// error plus the number of attempts made, so the caller can name the count
// in its own terminal message without this package holding any
// journal-writing dependency.
func mintWithRetry(ctx context.Context, attempt func(ctx context.Context) (retryable bool, err error)) (attempts int, err error) {
	var lastErr error
	for n := 0; n < mintMaxAttempts; n++ {
		attempts = n + 1
		retryable, attemptErr := attempt(ctx)
		if attemptErr == nil {
			return attempts, nil
		}
		lastErr = attemptErr
		if !retryable || attempts == mintMaxAttempts {
			return attempts, lastErr
		}
		timer := time.NewTimer(mintBackoff(mintRetryBaseDelay, mintRetryMaxDelay, n))
		select {
		case <-ctx.Done():
			timer.Stop()
			return attempts, fmt.Errorf("%w (context %w after %d attempt(s))", lastErr, ctx.Err(), attempts)
		case <-timer.C:
		}
	}
	return attempts, lastErr
}
