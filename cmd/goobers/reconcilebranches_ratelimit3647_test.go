package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

// exhaustedADORateLimit returns the error an ADO (or Gitea) call surfaces once
// its in-request backoff budget is spent on an HTTP 429: a generic non-2xx
// response error rather than a typed *providers.RateLimitError.
func exhaustedADORateLimit(t *testing.T) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)

	provider := providers.NewADOProvider("org", "project", "token",
		func(p *providers.ADOProvider) { p.BaseURL = server.URL },
		providers.WithADOMaxRateLimitRetries(0),
	)
	_, err := provider.GetWorkItem(context.Background(), providers.RepositoryRef{Project: "project"}, "42")
	if err == nil {
		t.Fatal("GetWorkItem() error = nil, want an exhausted rate-limit failure")
	}
	if _, ok := providers.AsRateLimitError(err); !ok {
		t.Fatalf("AsRateLimitError(%v) = false, want the 429 recognized", err)
	}
	return err
}

// TestReconcileRemoteBranchesJournalsUntypedSweepRateLimit is #3647's
// reconcile-side regression: an exhausted ADO/Gitea 429 must abort the sweep
// for quota backoff and journal the stable rate-limit code, exactly like the
// typed GitHub give-up already did — not fall through as a generic
// provider-lookup failure that the sweep keeps retrying.
func TestReconcileRemoteBranchesJournalsUntypedSweepRateLimit(t *testing.T) {
	logDir, log := openBranchReconcileLog(t)
	rateLimit := exhaustedADORateLimit(t)
	provider := &fakeBranchReconcileProvider{listErr: rateLimit}

	_, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Name: "app"},
		RunsDir:    t.TempDir(),
		Prefix:     branchReconcilePrefix,
		Limit:      25,
		MinimumAge: 7 * 24 * time.Hour,
	})
	if !errors.Is(err, rateLimit) {
		t.Fatalf("error = %v, want the sweep aborted with the rate limit", err)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 1 || events[0].Runner["event"] != "sweep" ||
		events[0].Runner["reason"] != "rate-limited" ||
		events[0].Error == nil || events[0].Error.Code != providers.ErrorCodeRateLimited {
		t.Fatalf("events = %+v", events)
	}
}
