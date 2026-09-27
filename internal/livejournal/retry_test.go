package livejournal

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fastRetryPolicy() RetryPolicy {
	return RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	attempts := 0
	err := withRetryPolicy(context.Background(), time.Second, fastRetryPolicy(), func(context.Context) (bool, error) {
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
	err := withRetryPolicy(context.Background(), time.Second, fastRetryPolicy(), func(context.Context) (bool, error) {
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
