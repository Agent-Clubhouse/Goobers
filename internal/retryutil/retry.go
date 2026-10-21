// Package retryutil provides shared retry loops and jittered exponential backoff.
package retryutil

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// ErrDeadlineExceeded marks a retry loop stopped by its elapsed-time bound.
var ErrDeadlineExceeded = errors.New("retry deadline exceeded")

// Policy controls exponential retry pacing.
type Policy struct {
	Base time.Duration
	Max  time.Duration

	// FullJitter samples from zero through the capped exponential delay.
	FullJitter bool

	// SymmetricJitter selects a ±25% window around the capped exponential
	// delay instead of the default half-to-full window. Samples above Max
	// are clamped to Max.
	SymmetricJitter bool

	// randInt64N is a test hook that defaults to rand.Int64N when nil.
	randInt64N func(int64) int64
}

// JitteredExponential returns a duration between half and all of
// Base<<attempt, with the exponential ceiling capped at Max.
// With SymmetricJitter, it instead samples within ±25% of that ceiling,
// still returning at most Max.
// If Base <= 0, Max is used as the ceiling. If Max <= 0, it returns zero.
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
	if p.FullJitter {
		floor = 0
		width = ceiling
	} else if p.SymmetricJitter {
		quarter := ceiling / 4
		floor = ceiling - quarter
		width = 2 * quarter
	}
	randInt64N := p.randInt64N
	if randInt64N == nil {
		randInt64N = rand.Int64N
	}
	jitter := time.Duration(randInt64N(int64(width) + 1))
	// Check before adding so a symmetric sample cannot overflow Duration.
	if jitter > p.Max-floor {
		return p.Max
	}
	return floor + jitter
}

// Until runs attempt until it succeeds, reports a non-retryable failure, or
// deadline elapses, waiting according to p between retryable failures.
// The deadline bounds the whole loop; an earlier deadline on ctx takes precedence.
func Until(ctx context.Context, deadline time.Duration, p Policy, attempt func(context.Context) (retryable bool, err error)) error {
	if deadline <= 0 {
		return fmt.Errorf("%w before first attempt", ErrDeadlineExceeded)
	}
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
			return retryDeadlineError(ctx.Err(), n+1, err)
		}
		previousErr = err
		timer := time.NewTimer(JitteredExponential(p, n))
		select {
		case <-ctx.Done():
			timer.Stop()
			return retryDeadlineError(ctx.Err(), n+1, err)
		case <-timer.C:
		}
	}
}

func retryDeadlineError(ctxErr error, attempts int, err error) error {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w after %d attempt(s): %w", ErrDeadlineExceeded, attempts, err)
	}
	return fmt.Errorf("retry canceled after %d attempt(s): %w", attempts, err)
}
