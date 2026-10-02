package retryutil

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestJitteredExponential(t *testing.T) {
	const (
		base = 10 * time.Second
		max  = 100 * time.Second
	)
	tests := []struct {
		name    string
		attempt int
		rand    int64
		want    time.Duration
	}{
		{name: "floor", attempt: 2, want: 20 * time.Second},
		{name: "ceiling", attempt: 2, rand: int64(20 * time.Second), want: 40 * time.Second},
		{name: "capped floor", attempt: 4, want: 50 * time.Second},
		{name: "capped ceiling", attempt: 4, rand: int64(50 * time.Second), want: 100 * time.Second},
		{name: "overflow", attempt: 100, want: 50 * time.Second},
		{name: "negative shift", attempt: -1, want: 50 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := JitteredExponential(Policy{
				Base: base,
				Max:  max,
				RandInt63n: func(n int64) int64 {
					if tt.rand >= n {
						t.Fatalf("random value %d outside bound %d", tt.rand, n)
					}
					return tt.rand
				},
			}, tt.attempt)
			if got != tt.want {
				t.Fatalf("JitteredExponential attempt %d = %s, want %s", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestJitteredExponentialInvalidPolicyReturnsZero(t *testing.T) {
	if got := JitteredExponential(Policy{}, 0); got != 0 {
		t.Fatalf("JitteredExponential with empty policy = %s, want 0", got)
	}
}

func TestUntilStopsOnNonRetryableError(t *testing.T) {
	want := errors.New("permanent")
	attempts := 0
	err := Until(context.Background(), time.Second, Policy{}, func(context.Context) (bool, error) {
		attempts++
		return false, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Until error = %v, want %v", err, want)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestUntilSucceedsAfterRetryableError(t *testing.T) {
	attempts := 0
	err := Until(context.Background(), time.Second, Policy{}, func(context.Context) (bool, error) {
		attempts++
		if attempts == 1 {
			return true, errors.New("transient")
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Until error = %v, want nil", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestUntilContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	want := errors.New("transient")
	attempts := 0
	err := Until(ctx, time.Minute, Policy{Base: time.Hour, Max: time.Hour}, func(context.Context) (bool, error) {
		attempts++
		cancel()
		return true, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Until error = %v, want it to wrap %v", err, want)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestUntilWrapsDeadlineError(t *testing.T) {
	want := errors.New("transient")
	err := Until(context.Background(), time.Nanosecond, Policy{Base: time.Hour, Max: time.Hour}, func(context.Context) (bool, error) {
		return true, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Until error = %v, want it to wrap %v", err, want)
	}
	const attempts = 1
	wantText := fmt.Sprintf("retry deadline exceeded after %d attempt(s): %v", attempts, want)
	if err.Error() != wantText {
		t.Fatalf("Until error = %q, want %q", err, wantText)
	}
}
