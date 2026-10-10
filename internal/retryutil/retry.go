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
}

// JitteredExponential returns a duration between half and all of
// Base<<attempt, with the exponential ceiling capped at Max.
func JitteredExponential(p Policy, attempt int) time.Duration {
	ceiling := p.Max
	if attempt >= 0 && p.Base > 0 && p.Max > 0 && attempt < 63 && p.Base <= p.Max>>attempt {
		ceiling = p.Base << attempt
	}
	if ceiling <= 0 {
		return 0
	}
	floor := ceiling / 2
	randInt63n := p.RandInt63n
	if randInt63n == nil {
		randInt63n = rand.Int64N
	}
	return floor + time.Duration(randInt63n(int64(ceiling-floor)+1))
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
