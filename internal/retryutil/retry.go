// Package retryutil provides shared retry loops and jittered exponential backoff.
package retryutil

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// Policy controls exponential retry pacing.
type Policy struct {
	Base time.Duration
	Max  time.Duration

	// randInt64N is a test hook that defaults to rand.Int64N when nil.
	randInt64N func(int64) int64
}

// JitteredExponential returns a duration between half and all of
// Base<<attempt, with the exponential ceiling capped at Max.
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
	randInt64N := p.randInt64N
	if randInt64N == nil {
		randInt64N = rand.Int64N
	}
	return floor + time.Duration(randInt64N(int64(ceiling-floor)+1))
}

// Until runs attempt until it succeeds, reports a non-retryable failure, or
// deadline elapses, waiting according to p between retryable failures.
// The deadline bounds the whole loop; an earlier deadline on ctx takes precedence.
func Until(ctx context.Context, deadline time.Duration, p Policy, attempt func(context.Context) (retryable bool, err error)) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	for n := 0; ; n++ {
		retryable, err := attempt(ctx)
		if err == nil {
			return nil
		}
		if !retryable {
			return err
		}
		timer := time.NewTimer(JitteredExponential(p, n))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retry deadline exceeded after %d attempt(s): %w", n+1, err)
		case <-timer.C:
		}
	}
}
