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

func TestVerifyCycleRetriesARetryableFailureInlineAndStopsOnPermanentOnes(t *testing.T) {
	a, b := node(providers.ProviderGitHub, "1"), node(providers.ProviderGitHub, "2")
	delays := []time.Duration{time.Millisecond, time.Millisecond}

	// A transient failure that recovers on a retry is verified.
	p := &probe{closed: map[string]bool{"1": true}, failing: map[string]bool{"1": true}}
	reads := 0
	store := Store{RetryDelays: delays, Closed: func(ctx context.Context, run string, repo providers.RepositoryRef, id string) (bool, error) {
		reads++
		if reads == 2 {
			p.failing["1"] = false
		}
		return p.closedFunc(ctx, run, repo, id)
	}}
	closed, skipped, unverifiable := store.verifyCycle(t.Context(), nil, []blockedcycle.Node{a}, "run")
	if len(closed) != 1 || len(skipped) != 0 || len(unverifiable) != 0 || reads != 2 {
		t.Fatalf("recovering retry: closed %v skipped %v unverifiable %v after %d reads", closed, skipped, unverifiable, reads)
	}

	// A failure that persists is read once plus once per delay, then reported.
	p = &probe{failing: map[string]bool{"1": true}}
	store = Store{RetryDelays: delays, Closed: p.closedFunc}
	closed, _, unverifiable = store.verifyCycle(t.Context(), nil, []blockedcycle.Node{a}, "run")
	if len(closed) != 0 || len(unverifiable) != 1 || len(p.calls) != 1+len(delays) {
		t.Fatalf("persistent failure: closed %v unverifiable %v after %d reads, want %d", closed, unverifiable, len(p.calls), 1+len(delays))
	}

	// A permanent failure is never retried.
	perm := 0
	store = Store{RetryDelays: delays, Closed: func(context.Context, string, providers.RepositoryRef, string) (bool, error) {
		perm++
		return false, &VerifyError{Class: "not found", Permanent: true, Message: "gone"}
	}}
	_, _, unverifiable = store.verifyCycle(t.Context(), nil, []blockedcycle.Node{a, b}, "run")
	if perm != 2 || len(unverifiable) != 2 || !unverifiable[0].Permanent {
		t.Fatalf("permanent failures: %d reads, %v; want each read once", perm, unverifiable)
	}

	// Only the failing member is re-read, and a cancelled context ends the waiting.
	p = &probe{closed: map[string]bool{"2": true}, failing: map[string]bool{"1": true}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	store = Store{RetryDelays: []time.Duration{time.Hour}, Closed: p.closedFunc}
	closed, _, unverifiable = store.verifyCycle(ctx, nil, []blockedcycle.Node{a, b}, "run")
	if len(closed) != 1 || closed[0] != b || len(unverifiable) != 1 || len(p.calls) != 2 {
		t.Fatalf("cancelled wait: closed %v unverifiable %v reads %v; want no retry and no hang", closed, unverifiable, p.calls)
	}
}

func TestUnverifiedNotesNameEveryUnconfirmedMember(t *testing.T) {
	notes := unverifiedNotes(
		[]blockedcycle.Node{node(providers.ProviderGitHub, "3")},
		[]unverified{{Node: node(providers.ProviderGitHub, "4"), Class: "not found"}})
	want := []string{"#3: unverifiable: rate limited (provider read budget spent)", "#4: unverifiable: not found"}
	if len(notes) != 2 || notes[0] != want[0] || notes[1] != want[1] {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
	if len(unverifiedNotes(nil, nil)) != 0 {
		t.Fatal("a fully verified cycle carries notes")
	}
}
