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

func TestMarkSuspendedSinceSubtractsActiveTime(t *testing.T) {
	wall := time.Unix(1_000, 0)
	active := 10 * time.Minute
	mark := newMarkWithSources(func() time.Time { return wall }, func() (time.Duration, error) { return active, nil })

	// Five active minutes plus a two-hour suspend: the monotonic clock moved
	// 2h5m, the unbiased clock only 5m.
	now := wall.Add(2*time.Hour + 5*time.Minute)
	got := mark.suspendedSinceWithSources(
		func() time.Time { return now },
		func() (time.Duration, error) { return active + 5*time.Minute, nil },
	)
	if got != 2*time.Hour {
		t.Fatalf("SuspendedSince = %s, want 2h", got)
	}
}

func TestMarkSuspendedSinceIgnoresClockGranularity(t *testing.T) {
	wall := time.Unix(1_000, 0)
	mark := newMarkWithSources(func() time.Time { return wall }, func() (time.Duration, error) { return time.Minute, nil })
	got := mark.suspendedSinceWithSources(
		func() time.Time { return wall.Add(10*time.Minute + 16*time.Millisecond) },
		func() (time.Duration, error) { return 11 * time.Minute, nil },
	)
	if got != 0 {
		t.Fatalf("SuspendedSince = %s, want 0 below the noise floor", got)
	}
}

func TestMarkSuspendedSinceReportsZeroWhenClockUnavailable(t *testing.T) {
	failing := func() (time.Duration, error) { return 0, errors.New("unavailable") }
	wall := time.Unix(1_000, 0)
	if got := newMarkWithSources(func() time.Time { return wall }, failing).SuspendedSince(); got != 0 {
		t.Fatalf("mark taken without the unbiased clock reported %s suspended", got)
	}
	mark := newMarkWithSources(func() time.Time { return wall }, func() (time.Duration, error) { return time.Minute, nil })
	if got := mark.suspendedSinceWithSources(func() time.Time { return wall.Add(time.Hour) }, failing); got != 0 {
		t.Fatalf("unavailable clock at read reported %s suspended", got)
	}
	if got := (Mark{}).SuspendedSince(); got != 0 {
		t.Fatalf("zero Mark reported %s suspended", got)
	}
}

func TestNewMarkUsesLiveClocks(t *testing.T) {
	mark := NewMark()
	if !mark.ok {
		t.Fatal("NewMark could not read the unbiased interrupt time")
	}
	if got := mark.SuspendedSince(); got != 0 {
		t.Fatalf("SuspendedSince immediately after NewMark = %s, want 0", got)
	}
}
