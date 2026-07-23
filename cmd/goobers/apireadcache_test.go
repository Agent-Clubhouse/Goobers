package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

func apiReadGet(t *testing.T, c *apiReadCache, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func apiReadBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestAPIReadCacheConditionalGET is the core #1053 contract: a repeated GET
// sends If-None-Match and a 304 is transparently replayed from the cached body
// (with the Link pagination header preserved), and the cache persists to disk so
// a fresh process (a later tick / sibling stage) reuses the ETag.
func TestAPIReadCacheConditionalGET(t *testing.T) {
	const body = `[{"number":1}]`
	const etag = `"abc123"`
	const link = `<https://api.github.com/x?page=2>; rel="next"`

	var conditionalSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			conditionalSeen = true
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Link", link)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cache := newAPIReadCache(dir, "", &http.Client{})

	// First GET: full 200, stores the ETag + body.
	if got := apiReadBody(t, apiReadGet(t, cache, srv.URL, "tok")); got != body {
		t.Fatalf("first GET body = %q, want %q", got, body)
	}

	// Second GET (same URL + token): conditional request, 304 replayed from cache.
	resp := apiReadGet(t, cache, srv.URL, "tok")
	if got := apiReadBody(t, resp); got != body {
		t.Fatalf("replayed body = %q, want %q", got, body)
	}
	if !conditionalSeen {
		t.Fatal("second GET did not send If-None-Match (no conditional request reached the server)")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replayed status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Link") != link {
		t.Fatalf("Link header not replayed on 304: %q", resp.Header.Get("Link"))
	}

	// Cross-process: a fresh cache over the same dir loads the on-disk ETag and
	// still issues a conditional request — this is the cross-tick/cross-stage win.
	conditionalSeen = false
	fresh := newAPIReadCache(dir, "", &http.Client{})
	if got := apiReadBody(t, apiReadGet(t, fresh, srv.URL, "tok")); got != body {
		t.Fatalf("fresh-instance body = %q, want %q", got, body)
	}
	if !conditionalSeen {
		t.Fatal("fresh cache instance did not reuse the on-disk ETag")
	}
}

// TestAPIReadCacheReducesQuotaGETs quantifies the #1053 win: over N identical
// ticks against an unchanged resource, exactly ONE response is a quota-costing
// 200 and the other N-1 are 304s (which do not count against GitHub's primary
// REST quota). This is the before/after in one assertion: O(ticks) quota GETs
// become O(1).
func TestAPIReadCacheReducesQuotaGETs(t *testing.T) {
	const etag = `"stable"`
	var full, conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			conditional++
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full++
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	cache := newAPIReadCache(t.TempDir(), "", &http.Client{})
	const ticks = 10
	for i := 0; i < ticks; i++ {
		_ = apiReadBody(t, apiReadGet(t, cache, srv.URL, "tok"))
	}
	if full != 1 {
		t.Fatalf("quota-costing (200) GETs = %d, want 1 over %d ticks", full, ticks)
	}
	if conditional != ticks-1 {
		t.Fatalf("free (304) conditional GETs = %d, want %d", conditional, ticks-1)
	}
}

// TestAPIReadCacheTokenScoped proves a different credential can never replay
// another's cached body: the entry key includes an Authorization fingerprint, so
// a mismatched token is a cache miss (a full, unconditional GET).
func TestAPIReadCacheTokenScoped(t *testing.T) {
	const etag = `"tok-scope"`
	var lastConditional bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastConditional = r.Header.Get("If-None-Match") != ""
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	cache := newAPIReadCache(t.TempDir(), "", &http.Client{})
	_ = apiReadBody(t, apiReadGet(t, cache, srv.URL, "token-a")) // primes token-a

	lastConditional = false
	_ = apiReadBody(t, apiReadGet(t, cache, srv.URL, "token-b"))
	if lastConditional {
		t.Fatal("a different token must not send another token's If-None-Match")
	}

	lastConditional = false
	_ = apiReadBody(t, apiReadGet(t, cache, srv.URL, "token-a"))
	if !lastConditional {
		t.Fatal("the original token should still hit its own cached ETag")
	}
}

// TestAPIReadCacheOnlyCachesGET confirms mutations and other verbs bypass the
// cache entirely — never conditional, never stored.
func TestAPIReadCacheOnlyCachesGET(t *testing.T) {
	var conditionalSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			conditionalSeen = true
		}
		w.Header().Set("ETag", `"post"`)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	cache := newAPIReadCache(t.TempDir(), "", &http.Client{})
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
		resp, err := cache.Do(req)
		if err != nil {
			t.Fatalf("POST Do: %v", err)
		}
		_ = apiReadBody(t, resp)
	}
	if conditionalSeen {
		t.Fatal("a POST must never carry If-None-Match")
	}
}

// TestAPIReadCacheFailOpen: with no scheduler dir the wrapper is a pure
// pass-through — it still serves the request, just without caching.
func TestAPIReadCacheFailOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			t.Error("no cache dir means no conditional requests")
		}
		w.Header().Set("ETag", `"x"`)
		_, _ = io.WriteString(w, "body")
	}))
	defer srv.Close()

	cache := newAPIReadCache("", "", &http.Client{})
	for i := 0; i < 2; i++ {
		if got := apiReadBody(t, apiReadGet(t, cache, srv.URL, "tok")); got != "body" {
			t.Fatalf("pass-through body = %q, want %q", got, "body")
		}
	}
}

// TestAPIReadCacheNoETagNotCached: a 200 without an ETag is never stored, so a
// repeat is a fresh unconditional GET.
func TestAPIReadCacheNoETagNotCached(t *testing.T) {
	var conditionalSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			conditionalSeen = true
		}
		_, _ = io.WriteString(w, "no-etag")
	}))
	defer srv.Close()

	cache := newAPIReadCache(t.TempDir(), "", &http.Client{})
	_ = apiReadBody(t, apiReadGet(t, cache, srv.URL, "tok"))
	_ = apiReadBody(t, apiReadGet(t, cache, srv.URL, "tok"))
	if conditionalSeen {
		t.Fatal("a 200 without an ETag must not be cached")
	}
}

func TestAPIReadCacheSharesListSnapshotAcrossConsumers(t *testing.T) {
	const body = `[{"number":1},{"number":2},{"number":3}]`
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	url := srv.URL + "/repos/acme/app/issues?state=open"
	const consumers = 20
	results := make(chan string, consumers)
	var wg sync.WaitGroup
	for range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				results <- "request error: " + err.Error()
				return
			}
			req.Header.Set("Authorization", "Bearer tok")
			resp, err := newAPIReadCache(dir, "tick-1", &http.Client{}).Do(req)
			if err != nil {
				results <- "request error: " + err.Error()
				return
			}
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				results <- "body error: " + err.Error()
				return
			}
			results <- string(got)
		}()
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got != body {
			t.Errorf("snapshot body = %q, want %q", got, body)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider list reads = %d for %d concurrent consumers, want 1", got, consumers)
	}
}

func TestAPIReadCacheRefreshesListSnapshotConditionally(t *testing.T) {
	const (
		body = `[{"number":1}]`
		etag = `"stable"`
	)
	requests := 0
	conditional := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("If-None-Match") == etag {
			conditional++
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	url := srv.URL + "/repos/acme/app/pulls?state=open"
	if got := apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-1", &http.Client{}), url, "tok")); got != body {
		t.Fatalf("first snapshot body = %q, want %q", got, body)
	}
	if got := apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-2", &http.Client{}), url, "tok")); got != body {
		t.Fatalf("not-modified snapshot body = %q, want %q", got, body)
	}
	if got := apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-2", &http.Client{}), url, "tok")); got != body {
		t.Fatalf("shared second snapshot body = %q, want %q", got, body)
	}
	if requests != 2 || conditional != 1 {
		t.Fatalf("requests = %d, conditional = %d; want 2 and 1", requests, conditional)
	}
}

func TestAPIReadCachePersistsSnapshotBodiesOnce(t *testing.T) {
	const (
		body = `[{"number":1}]`
		etag = `"stable"`
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	url := srv.URL + "/repos/acme/app/pulls?state=open"
	_ = apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-1", &http.Client{}), url, "tok"))
	bodyFiles, err := os.ReadDir(filepath.Join(dir, apiReadCacheBodyDir))
	if err != nil || len(bodyFiles) != 1 {
		t.Fatalf("body store after first snapshot: files = %d, err = %v", len(bodyFiles), err)
	}
	before, err := bodyFiles[0].Info()
	if err != nil {
		t.Fatalf("stat first body file: %v", err)
	}
	_ = apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-2", &http.Client{}), url, "tok"))

	data, err := os.ReadFile(filepath.Join(dir, apiReadCacheFileName))
	if err != nil {
		t.Fatalf("read cache metadata: %v", err)
	}
	var disk struct {
		Entries map[string]apiReadCacheEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("unmarshal cache metadata: %v", err)
	}
	refs := map[string]bool{}
	for key, entry := range disk.Entries {
		if entry.Body != nil {
			t.Fatalf("entry %q persisted an inline body", key)
		}
		if entry.BodyRef == "" {
			t.Fatalf("entry %q has no body reference", key)
		}
		refs[entry.BodyRef] = true
	}
	if len(refs) != 1 {
		t.Fatalf("persisted body references = %d, want 1 shared by base and snapshots", len(refs))
	}
	files, err := os.ReadDir(filepath.Join(dir, apiReadCacheBodyDir))
	if err != nil {
		t.Fatalf("read body store: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("persisted body files = %d, want 1", len(files))
	}
	info, err := files[0].Info()
	if err != nil {
		t.Fatalf("stat body file: %v", err)
	}
	if !os.SameFile(before, info) {
		t.Fatal("304 snapshot refresh rewrote the content-addressed response body")
	}
	if info.Size() != int64(len(body)) {
		t.Fatalf("persisted body bytes = %d, want %d", info.Size(), len(body))
	}
}

func TestEvictAPIReadCacheBoundsUniqueBodyBytes(t *testing.T) {
	shared := []byte("12345")
	entries := map[string]apiReadCacheEntry{
		"base-a":     {Body: shared, Stored: 2},
		"snapshot-a": {Body: shared, Stored: 2, Snapshot: "tick-2"},
		"base-b":     {Body: []byte("6789"), Stored: 1},
	}

	got := evictAPIReadCacheToLimits(entries, 10, 7)
	if _, ok := got["base-a"]; !ok {
		t.Fatal("newest base entry was evicted")
	}
	if _, ok := got["snapshot-a"]; !ok {
		t.Fatal("snapshot sharing the retained body was evicted")
	}
	if _, ok := got["base-b"]; ok {
		t.Fatal("entry exceeding the unique response-byte bound was retained")
	}
}

func TestAPIReadCacheKeepsOverlappingListSnapshotsStable(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("ETag", `"one"`)
			_, _ = io.WriteString(w, `[{"number":1}]`)
			return
		}
		w.Header().Set("ETag", `"two"`)
		_, _ = io.WriteString(w, `[{"number":2}]`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	url := srv.URL + "/repos/acme/app/issues?state=open"
	if got := apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-1", &http.Client{}), url, "tok")); got != `[{"number":1}]` {
		t.Fatalf("first snapshot body = %q", got)
	}
	if got := apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-2", &http.Client{}), url, "tok")); got != `[{"number":2}]` {
		t.Fatalf("second snapshot body = %q", got)
	}
	if got := apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-1", &http.Client{}), url, "tok")); got != `[{"number":1}]` {
		t.Fatalf("reused first snapshot body = %q, want its original view", got)
	}
	if requests != 2 {
		t.Fatalf("provider requests = %d, want 2", requests)
	}
}

func TestAPIReadCacheListSnapshotDoesNotHideProviderErrors(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			w.Header().Set("ETag", `"old"`)
			_, _ = io.WriteString(w, `[{"number":1}]`)
		case 2:
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
		default:
			w.Header().Set("ETag", `"new"`)
			_, _ = io.WriteString(w, `[{"number":2}]`)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	url := srv.URL + "/repos/acme/app/issues?state=open"
	_ = apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-1", &http.Client{}), url, "tok"))

	failed := apiReadGet(t, newAPIReadCache(dir, "tick-2", &http.Client{}), url, "tok")
	if failed.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed refresh status = %d, want %d", failed.StatusCode, http.StatusServiceUnavailable)
	}
	_ = apiReadBody(t, failed)

	if got := apiReadBody(t, apiReadGet(t, newAPIReadCache(dir, "tick-2", &http.Client{}), url, "tok")); got != `[{"number":2}]` {
		t.Fatalf("retry body = %q, want fresh provider response", got)
	}
	if requests != 3 {
		t.Fatalf("provider requests = %d, want 3 (failed refresh must not mark the snapshot valid)", requests)
	}
}

func TestPRSelectAndSiblingContextShareProductionListSnapshot(t *testing.T) {
	const selected = 10
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(selected, "Selected PR")
	server.addOpenPR(selected, "goobers/implementation/run-10", "main", "head-10", "base-10", false, nil, []fakePRFile{
		{path: "cmd/goobers/main.go", status: "modified"},
	})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv(providersnapshot.EnvVar, "tick-1")

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := server.pullListRequestCount(); got != 1 {
		t.Fatalf("production pr-select to sibling-context list requests = %d, want 1", got)
	}
}

func TestAPIReadCacheSnapshotPreservesPullRequestFilteringAndOrder(t *testing.T) {
	const body = `[
		{"number":1,"title":"first","state":"open","head":{"ref":"goobers/implementation/one","sha":"a"},"base":{"ref":"main"},"user":{"login":"bot"}},
		{"number":2,"title":"second","state":"open","head":{"ref":"goobers/pr-remediation/two","sha":"b"},"base":{"ref":"main"},"user":{"login":"bot"}},
		{"number":3,"title":"third","state":"open","head":{"ref":"goobers/implementation/three","sha":"c"},"base":{"ref":"main"},"user":{"login":"bot"}}
	]`
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("ETag", `"prs"`)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	newProvider := func() *providers.GitHubProvider {
		return providers.NewGitHubProvider("tok",
			providers.WithHTTPClient(newAPIReadCache(dir, "tick-1", &http.Client{})),
			func(p *providers.GitHubProvider) { p.BaseURL = srv.URL },
		)
	}
	repo := providers.RepositoryRef{Owner: "acme", Name: "app"}
	implementation, err := newProvider().ListPullRequests(context.Background(), providers.ListPullRequestsRequest{
		Repository: repo, HeadPrefix: "goobers/implementation/", SkipCheckState: true,
	})
	if err != nil {
		t.Fatalf("ListPullRequests implementation: %v", err)
	}
	remediation, err := newProvider().ListPullRequests(context.Background(), providers.ListPullRequestsRequest{
		Repository: repo, HeadPrefix: "goobers/pr-remediation/", SkipCheckState: true,
	})
	if err != nil {
		t.Fatalf("ListPullRequests remediation: %v", err)
	}
	if len(implementation) != 2 || implementation[0].Number != 1 || implementation[1].Number != 3 {
		t.Fatalf("implementation snapshot = %+v, want PRs 1 then 3", implementation)
	}
	if len(remediation) != 1 || remediation[0].Number != 2 {
		t.Fatalf("remediation snapshot = %+v, want PR 2", remediation)
	}
	if requests != 1 {
		t.Fatalf("provider list reads = %d, want 1 shared raw snapshot", requests)
	}
}
