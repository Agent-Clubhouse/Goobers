//go:build windows

package activetime

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTimerFiresAfterActiveDuration(t *testing.T) {
	timer := newTimer(25 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-time.After(2 * time.Second):
		t.Fatal("active-time timer did not fire")
	}
}

func TestTimerStopPreventsFiring(t *testing.T) {
	timer := newTimer(time.Second)
	if !timer.Stop() {
		t.Fatal("first Stop returned false")
	}
	if timer.Stop() {
		t.Fatal("second Stop returned true")
	}

	select {
	case <-timer.C:
		t.Fatal("stopped active-time timer fired")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestUnbiasedUptimeAdvances(t *testing.T) {
	before, err := unbiasedUptime()
	if err != nil {
		t.Fatalf("unbiasedUptime: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	after, err := unbiasedUptime()
	if err != nil {
		t.Fatalf("unbiasedUptime: %v", err)
	}
	if after <= before {
		t.Fatalf("unbiased uptime did not advance: before=%s after=%s", before, after)
	}
}

func TestTimerExcludesElapsedWallTimeWithoutActiveTime(t *testing.T) {
	polls := make(chan time.Time)
	observed := make(chan struct{}, 3)
	var mu sync.Mutex
	uptime := time.Hour
	timer := newTimerWithSources(
		10*time.Second,
		func() (time.Duration, error) {
			mu.Lock()
			defer mu.Unlock()
			observed <- struct{}{}
			return uptime, nil
		},
		polls,
		func() {},
		func(time.Duration) (<-chan time.Time, func() bool) {
			t.Fatal("wall-clock fallback was used")
			return nil, nil
		},
	)
	defer timer.Stop()
	<-observed

	polls <- time.Now().Add(time.Hour)
	<-observed
	select {
	case <-timer.C:
		t.Fatal("timer fired while unbiased uptime was unchanged")
	default:
	}

	mu.Lock()
	uptime += 10 * time.Second
	mu.Unlock()
	polls <- time.Now().Add(2 * time.Hour)
	<-observed
	select {
	case <-timer.C:
	case <-time.After(time.Second):
		t.Fatal("timer did not fire after active duration elapsed")
	}
}

func TestTimerFallsBackForRemainingDurationWhenUptimeFails(t *testing.T) {
	polls := make(chan time.Time)
	fallback := make(chan time.Time, 1)
	fallbackStarted := make(chan struct{})
	calls := 0
	var remaining time.Duration
	timer := newTimerWithSources(
		10*time.Second,
		func() (time.Duration, error) {
			calls++
			switch calls {
			case 1:
				return time.Hour, nil
			case 2:
				return time.Hour + 4*time.Second, nil
			default:
				return 0, errors.New("unbiased clock unavailable")
			}
		},
		polls,
		func() {},
		func(timeout time.Duration) (<-chan time.Time, func() bool) {
			remaining = timeout
			close(fallbackStarted)
			return fallback, func() bool { return true }
		},
	)
	defer timer.Stop()

	polls <- time.Now()
	polls <- time.Now()
	<-fallbackStarted
	if remaining != 6*time.Second {
		t.Fatalf("fallback duration = %s, want 6s", remaining)
	}
	select {
	case <-timer.C:
		t.Fatal("timer fired immediately when unbiased uptime failed")
	default:
	}

	fallback <- time.Now()
	select {
	case <-timer.C:
	case <-time.After(time.Second):
		t.Fatal("timer did not fire from wall-clock fallback")
	}
}
