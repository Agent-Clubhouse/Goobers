package localscheduler

import (
	"sync"
	"time"
)

// ProviderQuotaGate reports whether a provider's rate-limit quota is
// currently known to be exhausted and, if so, when it resets. Admit reads it
// synchronously (a cheap in-memory read, under its own lock) — never a
// network call — to enforce the provider-quota circuit breaker (#712), the
// dispatch-side complement to #614's stage-side rate-limit handling.
type ProviderQuotaGate interface {
	Exhausted(now time.Time) (resetAt time.Time, exhausted bool)
}

// ProviderQuotaState is the concrete, event-driven ProviderQuotaGate: pushed
// to (not polled) the instant a stage reports a github_rate_limited failure
// (runner.Config.RateLimited) with the resetAt its typed RateLimitError
// carried (#614). A background /rate_limit poller was considered and
// rejected: polling on an interval leaves a lag window where the scheduler
// keeps dispatching doomed runs between polls — exactly the waste #712
// exists to eliminate. Reacting synchronously at the moment of the first
// failure closes that window to "within one tick", per the issue's own
// acceptance criterion.
//
// Built once at the daemon composition root and shared by pointer between
// the Runner (which writes to it via the RateLimited hook) and the
// Scheduler (which reads it via Admit, through SetProviderQuota/
// WithProviderQuota) — the two are constructed in different order at the
// composition root (the Runner exists before the Scheduler does), so a
// Scheduler-owned field can't serve as the shared state; a pointer handed to
// both can. Mirrors OpenPRCounter/OpenPRRefresher's existing
// interface+state-holder split for the same reason (#353).
type ProviderQuotaState struct {
	mu      sync.RWMutex
	resetAt time.Time
}

// NewProviderQuotaState returns state with no exhaustion recorded.
func NewProviderQuotaState() *ProviderQuotaState {
	return &ProviderQuotaState{}
}

// RecordExhausted extends the quota-exhausted window to resetAt. A ratchet,
// not an overwrite: an earlier or zero resetAt never shortens an
// already-recorded later one, so a stale or racing report (e.g. two
// concurrent runs both hitting 403 moments apart, or an out-of-order
// delivery) can't prematurely reopen dispatch while a later-observed reset
// is still pending. A zero resetAt is a no-op — RateLimitError.Reset is only
// known when GitHub's response carried the header (#614); an unknown reset
// carries no information to ratchet against.
func (s *ProviderQuotaState) RecordExhausted(resetAt time.Time) {
	if resetAt.IsZero() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if resetAt.After(s.resetAt) {
		s.resetAt = resetAt
	}
}

// Exhausted implements ProviderQuotaGate: true only while now is strictly
// before the recorded resetAt. Once now reaches resetAt, dispatch reopens
// automatically — no explicit "clear" step needed.
func (s *ProviderQuotaState) Exhausted(now time.Time) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.resetAt.IsZero() || !now.Before(s.resetAt) {
		return time.Time{}, false
	}
	return s.resetAt, true
}

// ResetAt reports the last recorded reset time regardless of whether it has
// already passed, and whether any exhaustion has ever been recorded. Unlike
// Exhausted (which Admit uses to gate dispatch), this lets a caller like
// `goobers status` (#712) distinguish "never exhausted" from "recovered a
// moment ago" for a clearer status line.
func (s *ProviderQuotaState) ResetAt() (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resetAt, !s.resetAt.IsZero()
}
