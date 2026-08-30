package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

// TestClassifyProviderError_TooManyRequestsResponse is #3647's acceptance: an
// exhausted 429 from a provider that surfaces it as a generic non-2xx response
// (ADO, Gitea) used to classify as network_error, so the run missed quota
// backoff. It must classify as the stable rate-limit code, retryable, with the
// response's reset window carried into the stage result file.
func TestClassifyProviderError_TooManyRequestsResponse(t *testing.T) {
	reset := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset.Unix()))
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	provider := providers.NewADOProvider("org", "project", "token",
		func(p *providers.ADOProvider) { p.BaseURL = server.URL },
		providers.WithADOMaxRateLimitRetries(0),
	)
	_, err := provider.GetWorkItem(context.Background(), providers.RepositoryRef{Project: "project"}, "42")
	if err == nil {
		t.Fatal("GetWorkItem() error = nil, want an exhausted rate-limit failure")
	}

	code, retryable, extra := classifyProviderError(err)
	if code != providers.ErrorCodeRateLimited {
		t.Fatalf("code = %q, want %q — a 429 is a quota failure, not a network error", code, providers.ErrorCodeRateLimited)
	}
	if !retryable {
		t.Fatal("retryable = false, want true — the quota window rolls over on the clock")
	}
	if got := extra["rateLimitReset"]; got != reset.Format(time.RFC3339) {
		t.Fatalf("rateLimitReset = %v, want %v", got, reset.Format(time.RFC3339))
	}
}

// TestClassifyProviderError_TooManyRequestsMessage covers the same failure once
// only the error's message text survives a subprocess boundary: still a rate
// limit, never network_error.
func TestClassifyProviderError_TooManyRequestsMessage(t *testing.T) {
	err := errors.New(`GET /api/work-items failed: status 429: rate limited (Retry-After="60")`)

	code, retryable, extra := classifyProviderError(err)
	if code != providers.ErrorCodeRateLimited {
		t.Fatalf("code = %q, want %q", code, providers.ErrorCodeRateLimited)
	}
	if !retryable {
		t.Fatal("retryable = false, want true")
	}
	if extra != nil {
		t.Fatalf("extra = %v, want nil — no reset is recoverable from the message alone", extra)
	}
}
