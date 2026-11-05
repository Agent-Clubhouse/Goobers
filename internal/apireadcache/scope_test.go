package apireadcache

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/providers"
)

type scopedTransport func(*http.Request) (*http.Response, error)

func (f scopedTransport) Do(r *http.Request) (*http.Response, error) { return f(r) }

func scopedRead(t *testing.T, client providers.HTTPClient, provider providers.ProviderKind, token, accept string) *http.Response {
	t.Helper()
	url := "https://api.example/repos/org/repo/pulls"
	if provider == providers.ProviderADO {
		url = "https://dev.example/org/project/_apis/git/repositories/repo/pullrequests?api-version=7.1"
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func TestScopedCacheSeparatesGagglesBindingsGenerationsAndRepresentations(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "one", Binding: "automation", Generation: "v1"}
	calls := 0
	inner := scopedTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": {`"one"`}}, Body: io.NopCloser(strings.NewReader(req.Header.Get("Accept")))}, nil
	})
	read := func(s Scope, token, accept string) {
		t.Helper()
		client := ScopedClient(dir, "tick", s, providers.ProviderGitHub, inner)
		if got := apiReadBody(t, scopedRead(t, client, providers.ProviderGitHub, token, accept)); got != accept {
			t.Fatalf("wrong representation %q", got)
		}
	}
	read(scope, "token", "json")
	read(scope, "token", "json")
	if calls != 1 {
		t.Fatal("same partition did not share", calls)
	}
	read(Scope{Gaggle: "two", Binding: "automation", Generation: "v1"}, "token", "json")
	read(Scope{Gaggle: "one", Binding: "interactive", Generation: "v1"}, "token", "json")
	read(Scope{Gaggle: "one", Binding: "automation", Generation: "v2"}, "token", "json")
	read(scope, "rotated", "json")
	read(scope, "token", "raw")
	if calls != 6 {
		t.Fatal("partition leaked", calls)
	}
	if err := InvalidateScopedSnapshot(dir, "tick", scope); err != nil {
		t.Fatal(err)
	}
	read(scope, "token", "json")
	if calls != 7 {
		t.Fatal("same scope not invalidated", calls)
	}
	read(Scope{Gaggle: "two", Binding: "automation", Generation: "v1"}, "token", "json")
	if calls != 7 {
		t.Fatal("another gaggle invalidated", calls)
	}
}
func TestScopedADOListsShareAndPreserveContinuation(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "one", Binding: "backlog", Generation: "v1"}
	const readers = 8
	var calls atomic.Int32
	// Hold the leader's upstream read until every reader has started, so the
	// others really overlap it instead of arriving after the snapshot is stored.
	firstFetch, releaseFetch := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFetch) }) }
	defer release()
	inner := scopedTransport(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(firstFetch)
			select {
			case <-releaseFetch:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Ms-Continuationtoken": {"next-page"}}, Body: io.NopCloser(strings.NewReader(`{"value":[]}`))}, nil
	})
	// Coalescing across separately constructed wrappers is the contract under
	// test; the production one-second fail-open budget is covered separately.
	lockBudget := coalescingLockBudget(t)
	start := make(chan struct{})
	var entered, wg sync.WaitGroup
	entered.Add(readers)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := ScopedClient(dir, "tick", scope, providers.ProviderADO, inner)
			client.(*apiReadCache).lockBudget = lockBudget
			<-start
			entered.Done()
			resp := scopedRead(t, client, providers.ProviderADO, "token", "json")
			if resp.Header.Get("X-MS-ContinuationToken") != "next-page" {
				t.Error("lost pagination")
			}
			_ = apiReadBody(t, resp)
		}()
	}
	close(start)
	select {
	case <-firstFetch:
	case <-t.Context().Done():
		t.Fatal("first snapshot fetch never started")
	}
	entered.Wait()
	release()
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream list reads = %d for %d concurrent readers of one evaluation, want 1", got, readers)
	}
}
func TestMissingScopeAndExplicitFreshReadsBypassSharedCache(t *testing.T) {
	calls := 0
	inner := scopedTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": {`"one"`}}, Body: io.NopCloser(strings.NewReader("body"))}, nil
	})
	dir := t.TempDir()
	for range 2 {
		_ = apiReadBody(t, scopedRead(t, ScopedClient(dir, "tick", Scope{}, providers.ProviderGitHub, inner), providers.ProviderGitHub, "token", "json"))
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	cache := ScopedClient(dir, "tick", Scope{Gaggle: "one", Binding: "automation", Generation: "v1"}, providers.ProviderGitHub, inner)
	for range 2 {
		req, _ := http.NewRequest(http.MethodGet, "https://api.example/repos/org/repo/pulls", nil)
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := cache.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = apiReadBody(t, resp)
	}
	if calls != 4 {
		t.Fatal("explicit fresh read replayed", calls)
	}
}
func TestScopedGitHubOptionPreservesConfiguredTransportAndDefault(t *testing.T) {
	scope := Scope{Gaggle: "one", Binding: "automation", Generation: "v1"}
	inner := scopedTransport(func(*http.Request) (*http.Response, error) { return nil, nil })
	p := providers.NewGitHubProvider("token", providers.WithHTTPClient(inner), ScopedGitHubOption(t.TempDir(), "", scope))
	cache, ok := p.Client.(*apiReadCache)
	if !ok || cache.inner == nil {
		t.Fatal("transport missing")
	}
	p = providers.NewGitHubProvider("token", ScopedGitHubOption(t.TempDir(), "", scope))
	if p.Client.(*apiReadCache).inner == nil {
		t.Fatal("default missing")
	}
}

func TestScopedInvalidationReachesReusedWrapper(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "one", Binding: "automation", Generation: "v1"}
	calls := 0
	inner := scopedTransport(func(*http.Request) (*http.Response, error) {
		calls++
		body := "before"
		if calls > 1 {
			body = "after"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	client := ScopedClient(dir, "tick", scope, providers.ProviderGitHub, inner)
	read := func() string { return apiReadBody(t, scopedRead(t, client, providers.ProviderGitHub, "token", "json")) }
	first, second := read(), read()
	if first != "before" || second != "before" || calls != 1 {
		t.Fatal("snapshot did not share", calls)
	}
	if err := InvalidateScopedSnapshot(dir, "tick", scope); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "after" || calls != 2 {
		t.Fatal("reused wrapper replayed invalidated response", got, calls)
	}
}

func TestScopedProviderKindsCannotReuseValidators(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "one", Binding: "automation", Generation: "v1"}
	calls := 0
	inner := scopedTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("If-None-Match") != "" {
			t.Error("validator crossed provider kind")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": {`"one"`}}, Body: io.NopCloser(strings.NewReader("body"))}, nil
	})
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		client := ScopedClient(dir, "", scope, provider, inner)
		// Deliberately use the identical URL and headers for distinct adapters.
		_ = apiReadBody(t, scopedRead(t, client, providers.ProviderGitHub, "token", "json"))
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
