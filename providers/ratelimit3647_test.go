package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAsRateLimitErrorFromADOExhaustedResponse is #3647's core regression: an
// ADO 429 that outlives the in-request backoff budget surfaces as the generic
// non-2xx response error, which classified as a plain network failure. It must
// now recover provider-neutrally as a typed rate limit carrying the response's
// Retry-After metadata.
func TestAsRateLimitErrorFromADOExhaustedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	provider := NewADOProvider("org", "project", "token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADOMaxRateLimitRetries(0),
	)
	_, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project"}, "42")
	if err == nil {
		t.Fatal("GetWorkItem() error = nil, want an exhausted rate-limit failure")
	}
	before := time.Now().UTC()
	limit, ok := AsRateLimitError(err)
	if !ok {
		t.Fatalf("AsRateLimitError(%v) = false, want an exhausted 429 to classify as a rate limit", err)
	}
	if limit.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", limit.Status)
	}
	if limit.RetryAfterRaw != "120" {
		t.Fatalf("RetryAfterRaw = %q, want the response's Retry-After preserved", limit.RetryAfterRaw)
	}
	if limit.Secondary {
		t.Fatal("Secondary = true, want false — only GitHub distinguishes an abuse limit")
	}
	wantReset := before.Add(120 * time.Second)
	if limit.Reset.Before(before) || limit.Reset.After(wantReset.Add(time.Minute)) {
		t.Fatalf("Reset = %s, want ~%s derived from Retry-After", limit.Reset, wantReset)
	}
	if !IsTransientError(err) {
		t.Fatal("IsTransientError = false, want an exhausted rate limit to stay retryable")
	}
}

// TestAsRateLimitErrorPrefersResetHeader proves an absolute reset window wins
// over the relative Retry-After when the response carries both.
func TestAsRateLimitErrorPrefersResetHeader(t *testing.T) {
	reset := time.Now().UTC().Add(45 * time.Minute).Truncate(time.Second)
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	resp.Header.Set("Retry-After", "30")
	resp.Header.Set("X-RateLimit-Remaining", "0")
	resp.Header.Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset.Unix()))
	err := newProviderResponseError(resp, http.MethodGet, "/api/work-items", []byte("rate limited"))

	limit, ok := AsRateLimitError(fmt.Errorf("list work items: %w", err))
	if !ok {
		t.Fatalf("AsRateLimitError(%v) = false, want true through a wrapping error", err)
	}
	if !limit.Reset.Equal(reset) {
		t.Fatalf("Reset = %s, want %s from X-RateLimit-Reset", limit.Reset, reset)
	}
	if limit.Remaining != 0 || !limit.RemainingKnown || limit.RemainingRaw != "0" {
		t.Fatalf("remaining = %d/%v/%q, want the known exhausted quota preserved", limit.Remaining, limit.RemainingKnown, limit.RemainingRaw)
	}
	if msg := limit.Error(); !strings.Contains(msg, "remaining 0") {
		t.Fatalf("message = %q, want a known-exhausted quota reported as remaining 0", msg)
	}
	if limit.Endpoint != "/api/work-items" {
		t.Fatalf("Endpoint = %q, want the failing endpoint preserved", limit.Endpoint)
	}
}

// TestAsRateLimitErrorPassesThroughTypedGiveUp keeps the pre-existing #614
// path unchanged: GitHub's typed give-up is returned as-is, not re-synthesized.
func TestAsRateLimitErrorPassesThroughTypedGiveUp(t *testing.T) {
	typed := &RateLimitError{Provider: ProviderGitHub, Endpoint: "/issues", Status: 403, Secondary: true}

	limit, ok := AsRateLimitError(fmt.Errorf("list issues: %w", typed))
	if !ok || limit != typed {
		t.Fatalf("AsRateLimitError() = %v, %v, want the typed give-up unchanged", limit, ok)
	}
}

// TestAsRateLimitErrorIgnoresOtherFailures proves the classifier stays narrow:
// a non-429 provider response, a plain error, and nil are not rate limits.
func TestAsRateLimitErrorIgnoresOtherFailures(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}}
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"untyped", errors.New("boom")},
		{"server error", newProviderResponseError(resp, http.MethodGet, "/issues", []byte("unicorn"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if limit, ok := AsRateLimitError(tc.err); ok {
				t.Fatalf("AsRateLimitError() = %v, true, want false", limit)
			}
		})
	}
}

// TestRateLimitErrorMessageNamesProvider proves the typed error reads as the
// forge that produced it, and stays provider-neutral when the forge is not
// identifiable (#3647).
func TestRateLimitErrorMessageNamesProvider(t *testing.T) {
	github := (&RateLimitError{Provider: ProviderGitHub, Endpoint: "/issues", Status: 403}).Error()
	if !strings.HasPrefix(github, "github rate limited") {
		t.Fatalf("message = %q, want the github prefix preserved", github)
	}
	neutral := (&RateLimitError{Endpoint: "/api/work-items", Status: 429}).Error()
	if !strings.HasPrefix(neutral, "provider rate limited") {
		t.Fatalf("message = %q, want a provider-neutral prefix", neutral)
	}
	if !strings.Contains(neutral, ErrorCodeRateLimited) {
		t.Fatalf("message = %q, want the stable rate-limit code carried", neutral)
	}
}

// TestGitHubGitPushRateLimitErrorNamesGitHub guards the diagnostics of the
// pre-existing git-push path: it builds the typed error directly rather than
// from a RateLimitEvent, so it must still name the forge instead of falling
// back to the provider-neutral wording (#3647).
func TestGitHubGitPushRateLimitErrorNamesGitHub(t *testing.T) {
	repo := RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "app"}
	limit := githubGitPushRateLimitError(repo, []byte("remote: error: 429 Too Many Requests"))
	if limit == nil {
		t.Fatal("githubGitPushRateLimitError() = nil, want a typed rate limit")
	}
	if limit.Provider != ProviderGitHub {
		t.Fatalf("Provider = %q, want %q", limit.Provider, ProviderGitHub)
	}
	if !strings.HasPrefix(limit.Error(), "github rate limited") {
		t.Fatalf("message = %q, want the github prefix", limit.Error())
	}
}

// TestRateLimitErrorReportsUnknownRemaining pins #5960: a limit whose
// remaining-quota header was absent or malformed must say so, not misreport
// an exhausted quota as "remaining 0".
func TestRateLimitErrorReportsUnknownRemaining(t *testing.T) {
	for _, header := range []string{"", "lots", "-3"} {
		t.Run("header="+header, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
			resp.Header.Set("Retry-After", "30")
			if header != "" {
				resp.Header.Set("X-RateLimit-Remaining", header)
			}
			limit, ok := AsRateLimitError(newProviderResponseError(resp, http.MethodGet, "/api/work-items", []byte("rate limited")))
			if !ok {
				t.Fatal("AsRateLimitError() = false, want true for a 429")
			}
			if limit.RemainingKnown {
				t.Fatalf("RemainingKnown = true for X-RateLimit-Remaining=%q, want unknown", header)
			}
			msg := limit.Error()
			if !strings.Contains(msg, "remaining unknown") || strings.Contains(msg, "remaining 0") {
				t.Fatalf("message = %q, want remaining unknown", msg)
			}
		})
	}

	known := (&RateLimitError{Provider: ProviderADO, Endpoint: "/x", Status: 429, Remaining: 0, RemainingKnown: true}).Error()
	if !strings.Contains(known, "remaining 0") {
		t.Fatalf("message = %q, want a known Remaining:0 reported as remaining 0", known)
	}
}

// TestRateLimitErrorFromCarriesRemainingKnown pins that the give-up error
// built from a backoff decision keeps the event's known/unknown distinction,
// for both the ADO and GitHub backoff planners.
func TestRateLimitErrorFromCarriesRemainingKnown(t *testing.T) {
	unknown := rateLimitErrorFrom(RateLimitEvent{Provider: ProviderADO, Endpoint: "/x", Status: 429})
	if unknown.RemainingKnown || !strings.Contains(unknown.Error(), "remaining unknown") {
		t.Fatalf("unknown = %#v (%q), want remaining unknown", unknown, unknown.Error())
	}
	exhausted := rateLimitErrorFrom(RateLimitEvent{Provider: ProviderADO, Endpoint: "/x", Status: 429, Remaining: 0, RemainingKnown: true})
	if !exhausted.RemainingKnown || !strings.Contains(exhausted.Error(), "remaining 0") {
		t.Fatalf("exhausted = %#v (%q), want known remaining 0", exhausted, exhausted.Error())
	}

	github := NewGitHubProvider("token")
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	resp.Header.Set("X-RateLimit-Remaining", "0")
	if _, ev := github.rateLimitPlan(resp, "/issues", 0); !ev.RemainingKnown || ev.Remaining != 0 {
		t.Fatalf("github event = %#v, want a known remaining 0", ev)
	}
	resp.Header.Del("X-RateLimit-Remaining")
	resp.Header.Set("Retry-After", "1")
	if _, ev := github.rateLimitPlan(resp, "/issues", 0); ev.RemainingKnown {
		t.Fatalf("github event = %#v, want remaining unknown without the header", ev)
	}
}
