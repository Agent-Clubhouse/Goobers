package localscheduler

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestProviderQuotaStateExhaustedWhileBeforeReset(t *testing.T) {
	s := NewProviderQuotaState()
	now := time.Now()
	resetAt := now.Add(5 * time.Minute)
	s.RecordExhausted(resetAt)

	if got, exhausted := s.Exhausted(now); !exhausted || !got.Equal(resetAt) {
		t.Fatalf("Exhausted(before reset) = %v, %v; want %v, true", got, exhausted, resetAt)
	}
	if _, exhausted := s.Exhausted(resetAt); exhausted {
		t.Fatal("Exhausted(at reset) should be false — now must be strictly before resetAt")
	}
	if _, exhausted := s.Exhausted(resetAt.Add(time.Second)); exhausted {
		t.Fatal("Exhausted(after reset) should be false")
	}
}

func TestProviderQuotaStateNeverExhaustedIsFalse(t *testing.T) {
	s := NewProviderQuotaState()
	if _, exhausted := s.Exhausted(time.Now()); exhausted {
		t.Fatal("fresh state must not report exhausted")
	}
	if resetAt, known := s.ResetAt(); known || !resetAt.IsZero() {
		t.Fatalf("fresh state ResetAt = %v, %v; want zero, false", resetAt, known)
	}
}

func TestProviderQuotaStateRecordExhaustedIsARatchet(t *testing.T) {
	s := NewProviderQuotaState()
	now := time.Now()
	later := now.Add(10 * time.Minute)
	earlier := now.Add(2 * time.Minute)

	s.RecordExhausted(later)
	s.RecordExhausted(earlier) // must NOT shrink the window
	if got, known := s.ResetAt(); !known || !got.Equal(later) {
		t.Fatalf("ResetAt after earlier report = %v, %v; want %v, true (ratchet must not shrink)", got, known, later)
	}

	evenLater := now.Add(20 * time.Minute)
	s.RecordExhausted(evenLater) // a genuinely later report DOES extend
	if got, known := s.ResetAt(); !known || !got.Equal(evenLater) {
		t.Fatalf("ResetAt after later report = %v, %v; want %v, true", got, known, evenLater)
	}
}

func TestProviderQuotaStateRecordExhaustedZeroIsNoOp(t *testing.T) {
	s := NewProviderQuotaState()
	s.RecordExhausted(time.Time{})
	if resetAt, known := s.ResetAt(); known || !resetAt.IsZero() {
		t.Fatalf("zero RecordExhausted should be a no-op, got %v, %v", resetAt, known)
	}
}

func TestProviderQuotaStateBudgetsProvidersIndependently(t *testing.T) {
	s := NewProviderQuotaState()
	now := time.Now()
	resetAt := now.Add(time.Hour)
	s.Record(apiv1.ProviderGitHub, 2, resetAt)
	s.Record(apiv1.ProviderADO, 1, resetAt)

	github := s.ReservePolls(apiv1.ProviderGitHub, now, 3)
	if !github.Known || github.Allowed != 2 || github.RemainingAfter != 0 {
		t.Fatalf("GitHub reservation = %+v, want 2 of 3 allowed with 0 remaining", github)
	}
	ado := s.ReservePolls(apiv1.ProviderADO, now, 2)
	if !ado.Known || ado.Allowed != 1 || ado.RemainingAfter != 0 {
		t.Fatalf("ADO reservation = %+v, want independent 1 of 2 allowance", ado)
	}
}

func TestProviderQuotaStateReopensAtResetOnce(t *testing.T) {
	s := NewProviderQuotaState()
	now := time.Now()
	resetAt := now.Add(time.Minute)
	s.Record(apiv1.ProviderGitHub, 0, resetAt)

	before := s.ReservePolls(apiv1.ProviderGitHub, now, 2)
	if before.Allowed != 0 || !before.Known || before.Reset {
		t.Fatalf("reservation before reset = %+v, want known exhausted window", before)
	}
	atReset := s.ReservePolls(apiv1.ProviderGitHub, resetAt, 2)
	if atReset.Allowed != 2 || !atReset.Reset || atReset.Known {
		t.Fatalf("reservation at reset = %+v, want full reopened allowance and reset decision", atReset)
	}
	after := s.ReservePolls(apiv1.ProviderGitHub, resetAt.Add(time.Second), 2)
	if after.Allowed != 2 || after.Reset || after.Known {
		t.Fatalf("reservation after reported reset = %+v, want unbounded allowance without duplicate reset", after)
	}
}

func TestProviderQuotaStateSameWindowCannotAddBudgetBack(t *testing.T) {
	s := NewProviderQuotaState()
	now := time.Now()
	resetAt := now.Add(time.Hour)
	s.Record(apiv1.ProviderGitHub, 2, resetAt)
	s.ReservePolls(apiv1.ProviderGitHub, now, 1)

	s.Record(apiv1.ProviderGitHub, 5, resetAt)
	decision := s.ReservePolls(apiv1.ProviderGitHub, now, 2)
	if decision.Allowed != 1 {
		t.Fatalf("stale higher observation restored budget: %+v", decision)
	}
}

func TestProviderQuotaStateRefundsCachedPoll(t *testing.T) {
	s := NewProviderQuotaState()
	now := time.Now()
	resetAt := now.Add(time.Hour)
	s.Record(apiv1.ProviderGitHub, 2, resetAt)
	budget := s.ReservePolls(apiv1.ProviderGitHub, now, 1)
	reservation, ok := budget.Reservation()
	if !ok {
		t.Fatalf("reservation = %+v, want active window token", budget)
	}
	s.RefundReservation(reservation)

	decision := s.ReserveCurrentPolls(apiv1.ProviderGitHub, 2)
	if decision.RemainingBefore != 2 || decision.Allowed != 2 || decision.RemainingAfter != 0 {
		t.Fatalf("reservation after cached refund = %+v, want both requests restored", decision)
	}
}

func TestProviderQuotaStateRejectsStaleCachedRefund(t *testing.T) {
	s := NewProviderQuotaState()
	now := time.Now()
	resetAt := now.Add(time.Hour)
	s.Record(apiv1.ProviderGitHub, 1, resetAt)
	budget := s.ReservePolls(apiv1.ProviderGitHub, now, 1)
	reservation, ok := budget.Reservation()
	if !ok {
		t.Fatalf("reservation = %+v, want active window token", budget)
	}

	s.Record(apiv1.ProviderGitHub, 0, resetAt)
	s.RefundReservation(reservation)
	decision := s.ReserveCurrentPolls(apiv1.ProviderGitHub, 1)
	if decision.Allowed != 0 || decision.RemainingBefore != 0 {
		t.Fatalf("stale cached refund reopened exhausted window: %+v", decision)
	}
}

// TestAdmitBlocksWhileProviderQuotaExhausted is #712's core acceptance
// criterion: with a provider-quota gate wired and reporting exhausted,
// Admit refuses dispatch — regardless of workflow-level readiness — and
// journals the skip with the "provider-quota" reason prefix the issue's
// acceptance criteria name explicitly. No claim/budget/parallelism slot is
// reserved (Admit returns before any of those checks run).
func TestAdmitBlocksWhileProviderQuotaExhausted(t *testing.T) {
	c := NewConditions()
	now := time.Now()
	resetAt := now.Add(5 * time.Minute)
	pq := NewProviderQuotaState()
	pq.RecordExhausted(resetAt)
	c.SetProviderQuota(pq)

	ok, reason := c.Admit("implementation", apiv1.ReadinessConditions{MaxConcurrentRuns: 10}, now)
	if ok {
		t.Fatal("expected Admit to refuse while provider quota is exhausted")
	}
	if !strings.HasPrefix(reason, ReasonProviderQuota) {
		t.Fatalf("reason = %q, want prefix %q", reason, ReasonProviderQuota)
	}
	if c.Active("implementation") != 0 {
		t.Fatalf("Active = %d, want 0 — a quota-skipped tick must not reserve a slot", c.Active("implementation"))
	}
}

// TestAdmitResumesAtProviderQuotaReset covers dispatch resuming automatically
// once now reaches the recorded reset — no explicit "clear" step, no
// operator action, per the issue's second acceptance criterion.
func TestAdmitResumesAtProviderQuotaReset(t *testing.T) {
	c := NewConditions()
	now := time.Now()
	resetAt := now.Add(time.Minute)
	pq := NewProviderQuotaState()
	pq.RecordExhausted(resetAt)
	c.SetProviderQuota(pq)

	if ok, _ := c.Admit("wf", apiv1.ReadinessConditions{}, now); ok {
		t.Fatal("expected refusal before reset")
	}
	if ok, reason := c.Admit("wf", apiv1.ReadinessConditions{}, resetAt); !ok {
		t.Fatalf("expected admit at reset time: %s", reason)
	}
}

// TestAdmitUnaffectedWithoutProviderQuotaWired confirms fail-open: a
// Conditions with no gate wired (the zero value, matching every other
// condition's "never fails a tick on missing wiring" contract) never
// refuses on this basis.
func TestAdmitUnaffectedWithoutProviderQuotaWired(t *testing.T) {
	c := NewConditions()
	if ok, reason := c.Admit("wf", apiv1.ReadinessConditions{}, time.Now()); !ok {
		t.Fatalf("expected admit with no provider-quota gate wired: %s", reason)
	}
}
