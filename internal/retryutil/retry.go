// Package retryutil provides shared retry loops and jittered exponential backoff.
package retryutil

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Policy controls exponential retry pacing. RandInt63n defaults to
// rand.Int64N when nil.
type Policy struct {
	Base       time.Duration
	Max        time.Duration
	RandInt63n func(int64) int64
	// SymmetricJitter selects a ±25% window around the capped exponential
	// delay instead of the default half-to-full window. Samples above Max
	// are clamped to Max.
	SymmetricJitter bool
}

// JitteredExponential returns a duration between half and all of
// Base<<attempt, with the exponential ceiling capped at Max.
// With SymmetricJitter, it instead samples within ±25% of that ceiling,
// still returning at most Max.
func JitteredExponential(p Policy, attempt int) time.Duration {
	ceiling := p.Max
	if attempt >= 0 && p.Base > 0 && p.Max > 0 && attempt < 63 && p.Base <= p.Max>>attempt {
		ceiling = p.Base << attempt
	}
	if ceiling <= 0 {
		return 0
	}
	floor := ceiling / 2
	width := ceiling - floor
	if p.SymmetricJitter {
		quarter := ceiling / 4
		floor = ceiling - quarter
		width = 2 * quarter
	}
	randInt63n := p.RandInt63n
	if randInt63n == nil {
		randInt63n = rand.Int64N
	}
	jitter := time.Duration(randInt63n(int64(width) + 1))
	// Check before adding so a symmetric sample cannot overflow Duration.
	if jitter > p.Max-floor {
		return p.Max
	}
	return floor + jitter
}

// Until runs attempt until it succeeds, reports a non-retryable failure, or
// deadline elapses, waiting according to p between retryable failures.
func Until(ctx context.Context, deadline time.Duration, p Policy, attempt func(context.Context) (retryable bool, err error)) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var previousErr error
	for n := 0; ; n++ {
		retryable, err := attempt(ctx)
		if err == nil {
			return nil
		}
		// A final request interrupted by the retry deadline must retain the
		// preceding server failure as well as the cancellation cause.
		if previousErr != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			err = errors.Join(previousErr, err)
		}
		if !retryable {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("retry deadline exceeded after %d attempt(s): %w", n+1, err)
		}
		previousErr = err
		timer := time.NewTimer(JitteredExponential(p, n))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retry deadline exceeded after %d attempt(s): %w", n+1, err)
		case <-timer.C:
		}
	}
}
