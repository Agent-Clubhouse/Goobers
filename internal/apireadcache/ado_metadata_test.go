package apireadcache

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestADOStateMetadataSharedAcrossStageProviders(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "team", Binding: "automation:backlog", Generation: "generation"}
	fixture := &adoPlanFixture{}
	now := time.Now().Truncate(providers.ADOStateMetadataTTL).Add(time.Minute)
	for _, evaluation := range []string{"run-a", "run-b", "run-c"} {
		p := adoPlanProvider(dir, evaluation, scope, "token", fixture)
		p.Client.(*apiReadCache).metadataClock = func() time.Time { return now }
		items, info, err := adoPlanRead(t.Context(), p)
		assertADOPlanItems(t, items, info, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.calls["states"] != 1 {
		t.Fatalf("three stage providers fetched states %d times", fixture.calls["states"])
	}
}

func metadataRequest(t *testing.T, project, itemType, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.test/org/"+project+"/_apis/wit/workitemtypes/"+itemType+"/states?api-version=7.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func metadataRead(t *testing.T, client providers.HTTPClient, req *http.Request) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
}

func TestADOStateMetadataCachePartitionsAndExpires(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "team", Binding: "automation:backlog", Generation: "generation"}
	fixture := &adoPlanFixture{}
	now := time.Now().Truncate(providers.ADOStateMetadataTTL).Add(time.Minute)
	client := func(s Scope) *apiReadCache {
		c := ScopedClient(dir, "", s, providers.ProviderADO, fixture).(*apiReadCache)
		c.metadataClock = func() time.Time { return now }
		return c
	}
	req := func() *http.Request { return metadataRequest(t, "project", "Task", "token") }
	metadataRead(t, client(scope), req())
	metadataRead(t, client(scope), req())
	for _, change := range []Scope{{Gaggle: "other", Binding: scope.Binding, Generation: scope.Generation}, {Gaggle: scope.Gaggle, Binding: "interactive:backlog", Generation: scope.Generation}, {Gaggle: scope.Gaggle, Binding: scope.Binding, Generation: "new-generation"}} {
		metadataRead(t, client(change), req())
	}
	metadataRead(t, client(scope), metadataRequest(t, "project", "Task", "rotated-token"))
	metadataRead(t, client(scope), metadataRequest(t, "other-project", "Task", "token"))
	metadataRead(t, client(scope), metadataRequest(t, "project", "Bug", "token"))
	if fixture.calls["states"] != 7 {
		t.Fatalf("partitioned reads=%d", fixture.calls["states"])
	}
	c := client(scope)
	key := "ado-state-metadata-v1:" + scopedRequestKey(scope.key(), req())
	entry, ok := c.lookupDisk(key)
	if !ok {
		t.Fatal("missing stored metadata")
	}
	for _, stored := range []time.Time{now.Add(-2 * providers.ADOStateMetadataTTL), now.Add(time.Hour)} {
		entry.Stored = stored.Unix()
		c.persist(map[string]apiReadCacheEntry{key: entry})
		metadataRead(t, client(scope), req())
	}
	if fixture.calls["states"] != 9 {
		t.Fatalf("expired/future entries were reused: reads=%d", fixture.calls["states"])
	}
}

func TestADOStateMetadataConcurrentReadersAndInvalidResponses(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "team", Binding: "automation:backlog", Generation: "generation"}
	fixture := &adoPlanFixture{}
	now := time.Now().Truncate(providers.ADOStateMetadataTTL).Add(time.Minute)
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			client := ScopedClient(dir, "", scope, providers.ProviderADO, fixture).(*apiReadCache)
			client.lockBudget = time.Minute
			client.metadataClock = func() time.Time { return now }
			metadataRead(t, client, metadataRequest(t, "project", "Task", "token"))
		})
	}
	wg.Wait()
	if fixture.calls["states"] != 1 {
		t.Fatalf("concurrent reads=%d", fixture.calls["states"])
	}
	for _, body := range []string{`{}`, `{"value":null}`, `{"value":[{"name":"Active"}]}`, `not json`} {
		if validADOStateMetadata([]byte(body)) {
			t.Fatalf("invalid metadata accepted: %s", body)
		}
	}
	if !validADOStateMetadata([]byte(`{"value":[{"name":"Active","category":"InProgress"}]}`)) {
		t.Fatal("valid metadata refused")
	}
}

func TestADOStateMetadataTTLDoesNotExtendAcrossLayers(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 4, 59, 0, time.UTC)
	entry := apiReadCacheEntry{Stored: start.Unix()}
	if !adoStateMetadataFresh(entry, start.Add(500*time.Millisecond)) {
		t.Fatal("fresh metadata missed")
	}
	if adoStateMetadataFresh(entry, start.Add(time.Second)) {
		t.Fatal("metadata survived its five-minute window")
	}
}

func TestADOStateMetadataOnlyCachesExactAuthorizedGET(t *testing.T) {
	scope := Scope{Gaggle: "team", Binding: "automation:backlog", Generation: "generation"}
	fixture := &adoPlanFixture{}
	c := ScopedClient(t.TempDir(), "", scope, providers.ProviderADO, fixture).(*apiReadCache)
	for _, path := range []string{"/org/project/_apis/wit/workitemtypes/Task/states/extra", "/org/project/_apis/git/workitemtypes/Task/states", "/org/project/_apis/wit/workitems/42"} {
		req := metadataRequest(t, "project", "Task", "token")
		req.URL.Path = path
		if c.adoStateMetadataRequest(req) {
			t.Fatalf("unrelated endpoint accepted: %s", path)
		}
	}
	req := metadataRequest(t, "project", "Task", "token")
	req.Header.Del("Authorization")
	if c.adoStateMetadataRequest(req) {
		t.Fatal("anonymous metadata shared")
	}
	// The normal cache path refuses conditional requests and request bodies
	// before dispatching metadata reuse.
	req = metadataRequest(t, "project", "Task", "token")
	req.Body = io.NopCloser(strings.NewReader("body"))
	if scopedCacheable(req) {
		t.Fatal("request body accepted")
	}
}

type metadataTestClient func(*http.Request) (*http.Response, error)

func (f metadataTestClient) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestADOStateMetadataDoesNotCacheFailures(t *testing.T) {
	for _, first := range []struct {
		name, body string
		status     int
	}{
		{"HTTP error", `{"message":"unavailable"}`, http.StatusServiceUnavailable},
		{"malformed success", `{"value":[{"name":"Active"}]}`, http.StatusOK},
	} {
		t.Run(first.name, func(t *testing.T) {
			calls := 0
			inner := metadataTestClient(func(req *http.Request) (*http.Response, error) {
				calls++
				body, status := `{"value":[{"name":"Active","category":"InProgress"}]}`, http.StatusOK
				if calls == 1 {
					body, status = first.body, first.status
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			scope := Scope{Gaggle: "team", Binding: "automation:backlog", Generation: "generation"}
			dir := t.TempDir()
			now := time.Now().Truncate(providers.ADOStateMetadataTTL).Add(time.Minute)
			for range 3 {
				c := ScopedClient(dir, "", scope, providers.ProviderADO, inner).(*apiReadCache)
				c.metadataClock = func() time.Time { return now }
				metadataRead(t, c, metadataRequest(t, "project", "Task", "token"))
			}
			if calls != 2 {
				t.Fatalf("failed response cached or successful response not reused: calls=%d", calls)
			}
		})
	}
}
