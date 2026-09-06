package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"
)

// shrinkRetryDelays swaps in test-scale backoff bounds for the duration of
// one test, restoring the production values on cleanup — the same seam
// blobWriteThroughBudget provides on the write-through budget, applied here
// so a multi-attempt retry test runs in milliseconds instead of seconds.
func shrinkRetryDelays(t *testing.T) {
	t.Helper()
	origBase, origMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = origBase, origMax })
}

func TestWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	shrinkRetryDelays(t)
	attempts := 0
	err := withRetry(context.Background(), time.Second, func(context.Context) (bool, error) {
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
	shrinkRetryDelays(t)
	attempts := 0
	err := withRetry(context.Background(), time.Second, func(context.Context) (bool, error) {
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
	shrinkRetryDelays(t)
	deadline := 30 * time.Millisecond
	attempts := 0
	start := time.Now()
	err := withRetry(context.Background(), deadline, func(context.Context) (bool, error) {
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
	shrinkRetryDelays(t)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := withRetry(ctx, time.Minute, func(context.Context) (bool, error) {
		attempts++
		return true, errors.New("always fails")
	})
	if err == nil {
		t.Fatal("expected an error once the caller's context was cancelled")
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{200: false, 400: false, 401: false, 404: false, 409: false, 429: false, 500: true, 502: true, 503: true}
	for status, want := range cases {
		if got := retryableStatus(status); got != want {
			t.Errorf("retryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}
