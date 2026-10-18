package apireadcache

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

type scopedQuotaObserver struct {
	observations []providers.QuotaObservation
	delays       []providers.RateLimitEvent
}

func (o *scopedQuotaObserver) ObserveQuota(_ context.Context, v providers.QuotaObservation) {
	o.observations = append(o.observations, v)
}
func (o *scopedQuotaObserver) ObserveRateLimit(_ context.Context, v providers.RateLimitEvent) {
	o.delays = append(o.delays, v)
}

func TestScopedADOQuotaDistinguishesSnapshotAndConditionalRead(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "one", Binding: "automation", Generation: "generation"}
	calls := 0
	transport := scopedTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		status := http.StatusOK
		header := make(http.Header)
		header.Set("ETag", `"version"`)
		header.Set("X-RateLimit-Remaining", "10")
		header.Set("X-RateLimit-Reset", "2000000000")
		if req.Header.Get("If-None-Match") != "" {
			status = http.StatusNotModified
			header.Set("X-RateLimit-Remaining", "9")
			header.Set("X-RateLimit-Delay", "0.1")
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(`{"value":[]}`)), Request: req}, nil
	})
	observer := &scopedQuotaObserver{}
	provider := providers.NewADOProvider("org", "project", "token", providers.WithADOQuotaObserver(observer), providers.WithADORateLimitObserver(observer))
	provider.Client = ScopedClient(dir, "tick", scope, providers.ProviderADO, transport)
	read := func() {
		t.Helper()
		_, err := provider.ListOpenPullRequests(t.Context(), providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"})
		if err != nil {
			t.Fatal(err)
		}
	}
	read()
	read()
	if calls != 1 || len(observer.observations) != 2 {
		t.Fatal(calls, observer.observations)
	}
	first, cached := observer.observations[0], observer.observations[1]
	if first.Cached || !first.Known || first.Remaining != 10 || !cached.Cached || cached.Known {
		t.Fatal(observer.observations)
	}
	if err := InvalidateScopedSnapshot(dir, "tick", scope); err != nil {
		t.Fatal(err)
	}
	read()
	if calls != 2 || len(observer.observations) != 3 {
		t.Fatal(calls, observer.observations)
	}
	refreshed := observer.observations[2]
	if refreshed.Cached || !refreshed.Known || refreshed.Remaining != 9 || len(observer.delays) != 1 || observer.delays[0].Outcome != providers.RateLimitOutcomeDelayed {
		t.Fatal(refreshed, observer.delays)
	}
	read()
	if calls != 2 || len(observer.delays) != 1 {
		t.Fatal("cached replay retained network delay", calls, observer.delays)
	}
}
