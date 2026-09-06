package livejournal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// shrinkRetryDelays swaps in test-scale backoff bounds for the duration of
// one test, restoring the production values on cleanup — mirrors
// internal/dispatcher's copy of the same seam.
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

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{200: false, 400: false, 401: false, 404: false, 500: true, 503: true}
	for status, want := range cases {
		if got := retryableStatus(status); got != want {
			t.Errorf("retryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}
