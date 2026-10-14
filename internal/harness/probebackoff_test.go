package harness

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type probeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *probeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *probeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

func rateLimitedProbe(retryAfter time.Duration) error {
	return &TransientPreflightError{Err: errors.New("API rate limit exceeded"), RetryAfter: retryAfter}
}

func TestHarnessProbeBackoffSchedule(t *testing.T) {
	clock := &probeClock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	backoff := NewProbeBackoff(clock.Now)
	transient := rateLimitedProbe(0)

	// Without a Retry-After: 1m, doubling per consecutive failure, capped.
	var waits []time.Duration
	for range 7 {
		start := clock.Now()
		retryAt := backoff.Record("copilot", transient)
		waits = append(waits, retryAt.Sub(start))
		if _, waiting := backoff.Pending("copilot"); !waiting {
			t.Fatal("a just-recorded transient failure is not being waited out")
		}
		clock.Advance(retryAt.Sub(start) - time.Second)
		if _, waiting := backoff.Pending("copilot"); !waiting {
			t.Fatal("the backoff elapsed early")
		}
		clock.Advance(time.Second)
		if _, waiting := backoff.Pending("copilot"); waiting {
			t.Fatal("the backoff did not elapse at its retry time")
		}
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("backoff waits = %v, want %v", waits, want)
		}
	}

	// A provider Retry-After is a floor: longer than the backoff, it is the
	// wait; shorter, the backoff still applies.
	backoff.Clear("copilot")
	start := clock.Now()
	if retryAt := backoff.Record("copilot", rateLimitedProbe(90*time.Second)); retryAt.Sub(start) != 90*time.Second {
		t.Fatalf("retry-after wait = %s, want the provider's 90s over the 1m backoff", retryAt.Sub(start))
	}
	clock.Advance(90 * time.Second)
	start = clock.Now()
	if retryAt := backoff.Record("copilot", rateLimitedProbe(10*time.Second)); retryAt.Sub(start) != 2*time.Minute {
		t.Fatalf("short retry-after wait = %s, want the 2m backoff", retryAt.Sub(start))
	}

	// Another key is independent, and clear resets the schedule.
	if _, waiting := backoff.Pending("claude-code"); waiting {
		t.Fatal("an unrelated harness group was backed off")
	}
	backoff.Clear("copilot")
	start = clock.Now()
	if retryAt := backoff.Record("copilot", transient); retryAt.Sub(start) != time.Minute {
		t.Fatalf("wait after clear = %s, want the initial 1m", retryAt.Sub(start))
	}
}
