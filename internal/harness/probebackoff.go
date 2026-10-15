package harness

import (
	"sync"
	"time"
)

// A sign-in probe that failed because the provider could not answer (#6017)
// is not re-run until its backoff elapses: ProbeBackoffInitial doubling per
// consecutive transient failure up to ProbeBackoffMax, or the provider's
// Retry-After when that is longer.
const (
	ProbeBackoffInitial = time.Minute
	ProbeBackoffMax     = 30 * time.Minute
)

// ProbeBackoff records, per probe key, the transient failure that is being
// waited out and when the probe may run again. It is safe for concurrent use.
type ProbeBackoff struct {
	now func() time.Time

	mu      sync.Mutex
	waiting map[string]ProbeWait
}

// ProbeWait is one key's pending backoff.
type ProbeWait struct {
	// Cause is the transient failure being waited out.
	Cause error
	// RetryAt is when the probe may run again.
	RetryAt time.Time
	delay   time.Duration
}

// NewProbeBackoff returns an empty backoff on the given clock.
func NewProbeBackoff(now func() time.Time) *ProbeBackoff {
	return &ProbeBackoff{now: now, waiting: make(map[string]ProbeWait)}
}

// Pending returns the transient failure key is waiting out, if its backoff
// has not elapsed yet.
func (b *ProbeBackoff) Pending(key string) (ProbeWait, bool) {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	wait, ok := b.waiting[key]
	if !ok || !now.Before(wait.RetryAt) {
		return ProbeWait{}, false
	}
	return wait, true
}

// Due reports whether key may be probed at now.
func (b *ProbeBackoff) Due(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	wait, ok := b.waiting[key]
	return !ok || !now.Before(wait.RetryAt)
}

// Record schedules key's next probe after the transient failure cause and
// returns when that is.
func (b *ProbeBackoff) Record(key string, cause error) time.Time {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	delay := ProbeBackoffInitial
	if previous, ok := b.waiting[key]; ok {
		delay = min(2*previous.delay, ProbeBackoffMax)
	}
	// The provider's Retry-After is a floor, never a shortcut: a short one
	// must not let a host that keeps failing re-probe faster than its backoff.
	wait := delay
	if retryAfter, ok := TransientPreflightRetryAfter(cause); ok {
		wait = max(wait, retryAfter)
	}
	retryAt := now.Add(wait)
	b.waiting[key] = ProbeWait{Cause: cause, RetryAt: retryAt, delay: delay}
	return retryAt
}

// Clear forgets key's backoff: its last probe was not a transient failure.
func (b *ProbeBackoff) Clear(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.waiting, key)
}
