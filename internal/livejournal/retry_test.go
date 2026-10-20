package livejournal

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
