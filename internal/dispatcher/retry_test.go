package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/retryutil"
)

func fastRetryPolicy() RetryPolicy {
	return RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	attempts := 0
	err := retryutil.Until(context.Background(), time.Second, fastRetryPolicy().Backoff(), func(context.Context) (bool, error) {
		attempts++
		if attempts < 3 {
			return true, errors.New("transient")
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("withRetry: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestWithRetryStopsImmediatelyOnNonRetryableError(t *testing.T) {
	attempts := 0
	err := retryutil.Until(context.Background(), time.Second, fastRetryPolicy().Backoff(), func(context.Context) (bool, error) {
		attempts++
		return false, errors.New("permanent")
	})
	if err == nil {
		t.Fatal("expected the permanent error to surface")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no retry on a non-retryable failure)", attempts)
	}
}

func TestWithRetryHonoursDeadline(t *testing.T) {
	deadline := 30 * time.Millisecond
	attempts := 0
	start := time.Now()
	err := retryutil.Until(context.Background(), deadline, fastRetryPolicy().Backoff(), func(context.Context) (bool, error) {
		attempts++
		return true, errors.New("always fails")
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error once the retry deadline elapsed")
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d, want at least 2 (the deadline must allow more than one try)", attempts)
	}
	// Bounded, not exact: the loop must give up close to the deadline, not
	// run away — this is the regression check for issue #4260's second
	// acceptance criterion (the deadline is honoured, not merely documented).
	if elapsed > deadline+time.Second {
		t.Fatalf("elapsed = %s, want close to the %s deadline", elapsed, deadline)
	}
}

func TestWithRetryRespectsCallerContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := retryutil.Until(ctx, time.Minute, fastRetryPolicy().Backoff(), func(context.Context) (bool, error) {
		attempts++
		return true, errors.New("always fails")
	})
	if err == nil {
		t.Fatal("expected an error once the caller's context was cancelled")
	}
}
