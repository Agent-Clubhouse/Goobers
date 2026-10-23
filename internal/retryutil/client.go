package retryutil

import "time"

const (
	defaultClientBaseDelay = 500 * time.Millisecond
	defaultClientMaxDelay  = 30 * time.Second
)

// ClientPolicy overrides retry pacing for daemon HTTP clients. Nonpositive
// fields retain the production defaults of a 500ms base and a 30s maximum.
type ClientPolicy struct {
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

// Backoff applies the production defaults, then raises the maximum delay to
// at least the base delay.
func (p ClientPolicy) Backoff() Policy {
	base, max := p.BaseDelay, p.MaxDelay
	if base <= 0 {
		base = defaultClientBaseDelay
	}
	if max <= 0 {
		max = defaultClientMaxDelay
	}
	if max < base {
		max = base
	}
	return Policy{Base: base, Max: max}
}

// RetryableStatus reports whether a daemon HTTP response is retryable.
// Statuses >= 500 are treated as transient failures; lower statuses are not.
func RetryableStatus(statusCode int) bool {
	return statusCode >= 500
}
