package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/goobers/goobers/internal/credentials"
)

// transientStartupRetry bounds how long daemon startup waits out a transient
// provider failure (#5596) before failing as it always did. A provider quota
// window is typically under an hour; a misconfiguration that merely looks
// transient (an unreachable hostname) still surfaces as a startup failure once
// the budget is spent.
var transientStartupRetry = struct {
	initial, max, budget time.Duration
}{initial: 15 * time.Second, max: 5 * time.Minute, budget: time.Hour}

// retryTransientStartup runs build until it succeeds, fails with an error that
// is not a transient provider failure, the retry budget is spent, or ctx is
// done. Before #5596 a GitHub 403 from an exhausted quota while verifying a
// credential exited `goobers up`, leaving the instance down until someone
// restarted it by hand. build must release everything it acquired when it
// fails; buildSchedulerSetup does.
func retryTransientStartup[T any](ctx context.Context, stderr io.Writer, build func() (T, error)) (T, error) {
	delay := transientStartupRetry.initial
	deadline := time.Now().Add(transientStartupRetry.budget)
	for {
		value, err := build()
		if err == nil || !errors.Is(err, credentials.ErrTransientProvider) {
			return value, err
		}
		if ctx.Err() != nil {
			return value, errors.Join(err, ctx.Err())
		}
		if !time.Now().Add(delay).Before(deadline) {
			return value, err
		}
		pf(stderr, "warning: initialize daemon scheduler: %v; transient provider failure, retrying in %s\n", err, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return value, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		delay = min(2*delay, transientStartupRetry.max)
	}
}
