package localscheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ReasonProviderQuotaBudget identifies scheduler decisions caused by a
// provider's polling budget.
const ReasonProviderQuotaBudget = "provider-quota-budget"

// ProviderQuotaGate is the scheduler's view of provider quota. Exhausted gates
// run admission, while ReservePolls atomically budgets provider polling.
type ProviderQuotaGate interface {
	ExhaustedFor(provider apiv1.Provider, now time.Time) (resetAt time.Time, exhausted bool)
	ResetIfDue(provider apiv1.Provider, now time.Time) (ProviderQuotaReset, bool)
	ReservePolls(provider apiv1.Provider, now time.Time, requested int) ProviderPollBudget
}

// ProviderQuotaReset describes one quota window becoming inactive.
type ProviderQuotaReset struct {
	Provider  apiv1.Provider
	Remaining int
	ResetAt   time.Time
}

// ProviderPollBudget describes one tick's reservation against a provider
// window. Known is false when no active window is known, so all polls are
// admitted without inventing a quota.
type ProviderPollBudget struct {
	Provider        apiv1.Provider
	Requested       int
	Allowed         int
	RemainingBefore int
	RemainingAfter  int
	ResetAt         time.Time
	WindowVersion   uint64
	Known           bool
	Reset           bool
}

// ProviderPollReservation identifies one reservation in a specific observed
// provider window so cache refunds cannot reopen a subsequently ratcheted
// window.
type ProviderPollReservation struct {
	Provider      apiv1.Provider
	ResetAt       time.Time
	WindowVersion uint64
}

// Reservation returns a refundable token when the budget reserved quota from
// an active provider window.
func (b ProviderPollBudget) Reservation() (ProviderPollReservation, bool) {
	if !b.Known || b.Allowed <= 0 || b.WindowVersion == 0 {
		return ProviderPollReservation{}, false
	}
	return ProviderPollReservation{
		Provider:      b.Provider,
		ResetAt:       b.ResetAt,
		WindowVersion: b.WindowVersion,
	}, true
}

type providerPollReservationContextKey struct{}

// WithProviderPollBudget carries an admitted poll's reservation to its
// provider adapter.
func WithProviderPollBudget(ctx context.Context, budget ProviderPollBudget) context.Context {
	reservation, ok := budget.Reservation()
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, providerPollReservationContextKey{}, reservation)
}

// ProviderPollReservationFromContext returns the reservation for an admitted
// provider-backed poll.
func ProviderPollReservationFromContext(ctx context.Context) (ProviderPollReservation, bool) {
	reservation, ok := ctx.Value(providerPollReservationContextKey{}).(ProviderPollReservation)
	return reservation, ok
}

// ProviderPollBudgetError stops a provider-backed poll before an unbudgeted
// request, such as a pagination request, is issued.
type ProviderPollBudgetError struct {
	Provider  apiv1.Provider
	Remaining int
	Requested int
	ResetAt   time.Time
}

func (e *ProviderPollBudgetError) Error() string {
	return fmt.Sprintf("provider %s polling budget exhausted until %s", e.Provider, e.ResetAt.UTC().Format(time.RFC3339))
}

type providerQuotaWindow struct {
	remaining     int
	resetAt       time.Time
	resetReported bool
	version       uint64
}

// ProviderQuotaState is an event-driven, per-provider quota ledger shared by
// provider clients, the runner's rate-limit hook, and the scheduler.
type ProviderQuotaState struct {
	mu          sync.Mutex
	windows     map[apiv1.Provider]providerQuotaWindow
	nextVersion uint64
}

// NewProviderQuotaState returns an empty per-provider quota ledger.
func NewProviderQuotaState() *ProviderQuotaState {
	return &ProviderQuotaState{windows: make(map[apiv1.Provider]providerQuotaWindow)}
}

// Record stores an observed provider quota window. Observations within one
// window ratchet remaining downward so stale concurrent responses cannot add
// budget back. A later reset starts a new window; an earlier one is stale.
func (s *ProviderQuotaState) Record(provider apiv1.Provider, remaining int, resetAt time.Time) {
	if remaining < 0 || resetAt.IsZero() {
		return
	}
	provider = quotaProvider(provider)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.windows == nil {
		s.windows = make(map[apiv1.Provider]providerQuotaWindow)
	}
	current, ok := s.windows[provider]
	switch {
	case !ok || resetAt.After(current.resetAt):
		s.windows[provider] = providerQuotaWindow{
			remaining: remaining,
			resetAt:   resetAt,
			version:   s.nextWindowVersionLocked(),
		}
	case resetAt.Equal(current.resetAt) && !current.resetReported:
		if remaining < current.remaining {
			current.remaining = remaining
		}
		current.version = s.nextWindowVersionLocked()
		s.windows[provider] = current
	}
}

// RecordExhausted preserves the original GitHub circuit-breaker API.
func (s *ProviderQuotaState) RecordExhausted(resetAt time.Time) {
	s.Record(apiv1.ProviderGitHub, 0, resetAt)
}

// Exhausted preserves the original GitHub-only gate API.
func (s *ProviderQuotaState) Exhausted(now time.Time) (time.Time, bool) {
	return s.ExhaustedFor(apiv1.ProviderGitHub, now)
}

// ExhaustedFor reports whether a provider's active quota window has no
// remaining polling budget.
func (s *ProviderQuotaState) ExhaustedFor(provider apiv1.Provider, now time.Time) (time.Time, bool) {
	provider = quotaProvider(provider)
	s.mu.Lock()
	defer s.mu.Unlock()
	window, ok := s.windows[provider]
	if !ok || window.resetReported || !now.Before(window.resetAt) || window.remaining > 0 {
		return time.Time{}, false
	}
	return window.resetAt, true
}

// ResetIfDue retires an expired provider window exactly once.
func (s *ProviderQuotaState) ResetIfDue(provider apiv1.Provider, now time.Time) (ProviderQuotaReset, bool) {
	provider = quotaProvider(provider)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetIfDueLocked(provider, now)
}

// ReservePolls atomically reserves quota for provider polls. Once a reset is
// reached, the stale window is retired and reported exactly once; the scheduler
// then admits polling until a fresh observation arrives.
func (s *ProviderQuotaState) ReservePolls(provider apiv1.Provider, now time.Time, requested int) ProviderPollBudget {
	provider = quotaProvider(provider)
	decision := ProviderPollBudget{Provider: provider, Requested: requested}
	if requested <= 0 {
		return decision
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	window, ok := s.windows[provider]
	if !ok || window.resetReported {
		decision.Allowed = requested
		return decision
	}
	if reset, due := s.resetIfDueLocked(provider, now); due {
		decision.Allowed = requested
		decision.RemainingBefore = reset.Remaining
		decision.RemainingAfter = reset.Remaining
		decision.ResetAt = reset.ResetAt
		decision.Reset = true
		return decision
	}
	return s.reserveCurrentPollsLocked(provider, requested)
}

// ReserveCurrentPolls reserves more work in the active window without
// consulting a wall clock. A poll uses it for follow-up pagination requests
// after the scheduler has already processed the tick's reset transition.
func (s *ProviderQuotaState) ReserveCurrentPolls(provider apiv1.Provider, requested int) ProviderPollBudget {
	provider = quotaProvider(provider)
	if requested <= 0 {
		return ProviderPollBudget{Provider: provider, Requested: requested}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserveCurrentPollsLocked(provider, requested)
}

func (s *ProviderQuotaState) reserveCurrentPollsLocked(provider apiv1.Provider, requested int) ProviderPollBudget {
	decision := ProviderPollBudget{Provider: provider, Requested: requested}
	window, ok := s.windows[provider]
	if !ok || window.resetReported {
		decision.Allowed = requested
		return decision
	}
	decision.Known = true
	decision.ResetAt = window.resetAt
	decision.WindowVersion = window.version
	decision.RemainingBefore = window.remaining
	decision.Allowed = min(requested, window.remaining)
	window.remaining -= decision.Allowed
	decision.RemainingAfter = window.remaining
	s.windows[provider] = window
	return decision
}

// RefundReservation returns a reservation that the shared provider cache
// satisfied without consuming provider quota. A later quota observation
// invalidates the token, preventing a stale refund from reopening the window.
func (s *ProviderQuotaState) RefundReservation(reservation ProviderPollReservation) {
	if reservation.WindowVersion == 0 {
		return
	}
	provider := quotaProvider(reservation.Provider)
	s.mu.Lock()
	defer s.mu.Unlock()
	window, ok := s.windows[provider]
	if !ok || window.resetReported ||
		window.version != reservation.WindowVersion ||
		!window.resetAt.Equal(reservation.ResetAt) {
		return
	}
	window.remaining++
	s.windows[provider] = window
}

// ResetAt reports the last GitHub reset time for backward-compatible status
// inspection.
func (s *ProviderQuotaState) ResetAt() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	window, ok := s.windows[apiv1.ProviderGitHub]
	return window.resetAt, ok
}

func (s *ProviderQuotaState) resetIfDueLocked(provider apiv1.Provider, now time.Time) (ProviderQuotaReset, bool) {
	window, ok := s.windows[provider]
	if !ok || window.resetReported || now.Before(window.resetAt) {
		return ProviderQuotaReset{}, false
	}
	window.resetReported = true
	s.windows[provider] = window
	return ProviderQuotaReset{
		Provider:  provider,
		Remaining: window.remaining,
		ResetAt:   window.resetAt,
	}, true
}

func (s *ProviderQuotaState) nextWindowVersionLocked() uint64 {
	s.nextVersion++
	if s.nextVersion == 0 {
		s.nextVersion++
	}
	return s.nextVersion
}

func quotaProvider(provider apiv1.Provider) apiv1.Provider {
	if provider == "" {
		return apiv1.ProviderGitHub
	}
	return provider
}
