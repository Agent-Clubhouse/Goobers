package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestTransientPollCodeClassifiesUntypedRateLimit is #3647's ci-poll
// regression: providers that surface an exhausted quota as a plain HTTP 429
// response (ADO, Gitea) used to journal the generic poll_provider_error, so
// the run missed quota backoff and notified as if the forge had merely
// hiccuped.
func TestTransientPollCodeClassifiesUntypedRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
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
	if got := transientPollCode(err); got != providers.ErrorCodeRateLimited {
		t.Fatalf("transientPollCode(untyped 429) = %q, want %q", got, providers.ErrorCodeRateLimited)
	}
}
