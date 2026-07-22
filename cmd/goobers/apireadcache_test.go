package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
	cache := newAPIReadCache(dir, &http.Client{})

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
	fresh := newAPIReadCache(dir, &http.Client{})
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

	cache := newAPIReadCache(t.TempDir(), &http.Client{})
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

	cache := newAPIReadCache(t.TempDir(), &http.Client{})
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

	cache := newAPIReadCache(t.TempDir(), &http.Client{})
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

	cache := newAPIReadCache("", &http.Client{})
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

	cache := newAPIReadCache(t.TempDir(), &http.Client{})
	_ = apiReadBody(t, apiReadGet(t, cache, srv.URL, "tok"))
	_ = apiReadBody(t, apiReadGet(t, cache, srv.URL, "tok"))
	if conditionalSeen {
		t.Fatal("a 200 without an ETag must not be cached")
	}
}
