package blockedrecordspg

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

func TestClampDeferralBoundDefaultsAndNeverGoesBelowTheMinimum(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:                    DefaultDeferralBound,
		-time.Hour:           DefaultDeferralBound,
		time.Second:          MinDeferralBound,
		MinDeferralBound - 1: MinDeferralBound,
		MinDeferralBound:     MinDeferralBound,
		3 * time.Hour:        3 * time.Hour,
	} {
		if got := ClampDeferralBound(in); got != want {
			t.Errorf("ClampDeferralBound(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestCycleKeyIsOrderIndependentAndNamesTheMembers(t *testing.T) {
	a, b, c := node(providers.ProviderGitHub, "1"), node(providers.ProviderGitHub, "2"), node(providers.ProviderGitHub, "3")
	k := CycleKey([]blockedcycle.Node{a, b, c})
	if k != CycleKey([]blockedcycle.Node{c, a, b}) {
		t.Fatal("the same cycle found in another order got another key; a repeated escalation would not dedupe")
	}
	if k == CycleKey([]blockedcycle.Node{a, b}) || k == CycleKey([]blockedcycle.Node{a, b, node(providers.ProviderGitHub, "4")}) {
		t.Fatal("different cycles share a key")
	}
	other := b
	other.Repository.Name = "elsewhere"
	if k == CycleKey([]blockedcycle.Node{a, other, c}) {
		t.Fatal("the same item numbers in another repository share a key")
	}
}

func TestNextDelayDoublesCapsAndNeverOvershootsTheBound(t *testing.T) {
	for _, tc := range []struct {
		attempts  int
		remaining time.Duration
		want      time.Duration
	}{
		{1, time.Hour, FirstDeferralDelay},
		{2, time.Hour, 2 * FirstDeferralDelay},
		{3, time.Hour, 4 * FirstDeferralDelay},
		{4, time.Hour, MaxDeferralDelay},
		{9, time.Hour, MaxDeferralDelay},
		{1, 2 * time.Minute, 2 * time.Minute}, // the last re-check lands at the bound
		{2, 0, 2 * FirstDeferralDelay},
		{1, time.Millisecond, time.Second}, // never a busy loop
	} {
		if got := nextDelay(tc.attempts, tc.remaining); got != tc.want {
			t.Errorf("nextDelay(%d, %v) = %v, want %v", tc.attempts, tc.remaining, got, tc.want)
		}
	}
}

func TestAllPermanentRefusesOnlyWhenEveryFailureIsPermanent(t *testing.T) {
	perm := func(s string) error { return fmt.Errorf("%s: %w", s, providerwriter.ErrPermanent) }
	plain := errors.New("provider unavailable")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                        {nil, false},
		"plain":                      {plain, false},
		"one permanent":              {perm("a"), true},
		"not pinned":                 {fmt.Errorf("x: %w", providerwriter.ErrRepositoryNotPinned), true},
		"all permanent members":      {errors.Join(perm("a"), perm("b")), true},
		"permanent and transient":    {errors.Join(perm("a"), plain), false},
		"nested joins all permanent": {errors.Join(perm("a"), fmt.Errorf("wrap: %w", errors.Join(perm("b"), perm("c")))), true},
		"nested join with transient": {errors.Join(perm("a"), fmt.Errorf("wrap: %w", errors.Join(perm("b"), plain))), false},
		"empty multi-unwrap":         {emptyJoin{}, false},
	} {
		if got := allPermanent(tc.err); got != tc.want {
			t.Errorf("%s: allPermanent = %v, want %v", name, got, tc.want)
		}
	}
}

type emptyJoin struct{}

func (emptyJoin) Error() string   { return "empty" }
func (emptyJoin) Unwrap() []error { return nil }
