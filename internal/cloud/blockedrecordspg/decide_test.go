package blockedrecordspg

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

func TestClassifyTellsRetryableFromPermanent(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		class     string
		permanent bool
	}{
		"not pinned":           {fmt.Errorf("bind: %w", providerwriter.ErrRepositoryNotPinned), "repository not configured", true},
		"writer refusal":       {fmt.Errorf("x: %w", providerwriter.ErrPermanent), "refused", true},
		"rate limited":         {&providers.RateLimitError{Provider: providers.ProviderGitHub}, "rate limited", false},
		"deadline":             {fmt.Errorf("read: %w", context.DeadlineExceeded), "timed out", false},
		"server error":         {errors.New("GET /issues/7 failed: status 503: unavailable"), "provider unavailable", false},
		"unauthorized":         {errors.New("GET /issues/7 failed: status 401: bad credentials"), "unauthorized", true},
		"forbidden":            {errors.New("GET /issues/7 failed: status 403: forbidden"), "unauthorized", true},
		"gone (text only)":     {errors.New("get work item: GET /issues/7 failed: status 404: Not Found"), "not found", true},
		"unrecognised":         {errors.New("something odd"), "error", false},
		"kept after scrubbing": {&VerifyError{Class: "rate limited", Message: "[REDACTED]"}, "rate limited", false},
		"kept permanent":       {fmt.Errorf("wrapped: %w", &VerifyError{Class: "not found", Permanent: true, Message: "[REDACTED]"}), "not found", true},
	} {
		t.Run(name, func(t *testing.T) {
			class, permanent := classify(tc.err)
			if class != tc.class || permanent != tc.permanent {
				t.Fatalf("classify = %q, permanent %v; want %q, %v", class, permanent, tc.class, tc.permanent)
			}
		})
	}
}

func TestBackoffKeepsTheFirstFailureUntilTheItemVerifies(t *testing.T) {
	now := time.Unix(1000, 0)
	b := &Backoff{Now: func() time.Time { return now }}
	n := node(providers.ProviderGitHub, "7")
	if attempts, first := b.Failures(n); attempts != 0 || !first.IsZero() {
		t.Fatalf("fresh node failures = %d at %v", attempts, first)
	}
	b.Failed(n)
	started := now
	now = now.Add(30 * time.Minute)
	b.Failed(n)
	if attempts, first := b.Failures(n); attempts != 2 || !first.Equal(started) {
		t.Fatalf("failures = %d first %v; want 2, first kept at %v", attempts, first, started)
	}
	b.Verified(n)
	if attempts, first := b.Failures(n); attempts != 0 || !first.IsZero() {
		t.Fatalf("a verified node kept failures %d at %v", attempts, first)
	}
	var none *Backoff
	if attempts, _ := none.Failures(n); attempts != 0 {
		t.Fatal("a nil Backoff reported failures")
	}
}

func TestDecideEscalatesOnlyWhatIsVerifiedOrBoundedlyUnverifiable(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	cycle := blockedcycle.Result{Affected: []blockedcycle.Node{node(providers.ProviderGitHub, "1"), node(providers.ProviderGitHub, "2")}}
	member := cycle.Affected[1]

	store := Store{Backoff: &Backoff{Now: clock}}
	// Verified cycle: escalate, no notes.
	if got, err := store.decide(cycle, false, nil); err != nil || len(got.Affected) != 2 || len(got.Unverified) != 0 {
		t.Fatalf("verified cycle: %+v, %v", got, err)
	}
	// A spent read budget defers; nothing is escalated.
	if got, err := store.decide(cycle, true, nil); !errors.Is(err, ErrVerificationDeferred) || len(got.Affected) != 0 {
		t.Fatalf("budget spent: %+v, %v", got, err)
	}
	// A permanent failure escalates at once, naming the member and why.
	permanent := []unverified{{Node: member, Class: "not found", Permanent: true}}
	if got, err := store.decide(cycle, false, permanent); err != nil || len(got.Affected) != 2 || len(got.Unverified) != 1 || got.Unverified[0] != "#2: unverifiable: not found" {
		t.Fatalf("permanent failure: %+v, %v", got, err)
	}
	// A retryable failure is held: once failed, not until 2 attempts AND 1h.
	retry := []unverified{{Node: member, Class: "rate limited"}}
	store.Backoff.Failed(member)
	if _, err := store.decide(cycle, false, retry); !errors.Is(err, ErrVerificationDeferred) {
		t.Fatalf("first failure = %v, want deferred", err)
	}
	now = now.Add(2 * UnverifiedHold)
	if _, err := store.decide(cycle, false, retry); !errors.Is(err, ErrVerificationDeferred) {
		t.Fatalf("one attempt, long ago = %v, want deferred (needs %d attempts)", err, MinAttempts)
	}
	store.Backoff.Failed(member)
	if got, err := store.decide(cycle, false, retry); err != nil || len(got.Affected) != 2 || got.Unverified[0] != "#2: unverifiable: rate limited" {
		t.Fatalf("two attempts over the hold: %+v, %v; want the cycle escalated with the member named", got, err)
	}
	// Two attempts but inside the hold: still deferred.
	fresh := Store{Backoff: &Backoff{Now: clock}}
	fresh.Backoff.Failed(member)
	fresh.Backoff.Failed(member)
	if _, err := fresh.decide(cycle, false, retry); !errors.Is(err, ErrVerificationDeferred) {
		t.Fatalf("two quick attempts = %v, want deferred until the hour passes", err)
	}
	// One member held keeps the whole cycle held even beside a permanent one.
	mixed := append([]unverified{{Node: cycle.Affected[0], Class: "forbidden", Permanent: true}}, retry...)
	if _, err := fresh.decide(cycle, false, mixed); !errors.Is(err, ErrVerificationDeferred) {
		t.Fatalf("a held member beside a permanent one = %v, want deferred", err)
	}
	// Without a backoff there is nothing to wait on.
	if got, err := (Store{}).decide(cycle, false, retry); err != nil || len(got.Unverified) != 1 {
		t.Fatalf("no backoff: %+v, %v", got, err)
	}
}
