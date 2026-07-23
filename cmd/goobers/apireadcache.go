package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/goobers/goobers/providers"
)

// Baseline GitHub API READ-volume reduction (issue #1053).
//
// The daemon's read-heavy list stages — pr-select (merge-review),
// gather-pr-context (pr-remediation), backlog-query (backlog-curation) — each
// re-fetch the full open-PR / backlog collection every tick, uncached, so their
// primary-REST-quota cost scaled with backlog size rather than with what
// changed since the last tick. As the curator grew `ready` from 3 to 70 and
// open PRs past a dozen, that fixed per-tick floor crossed 5000 req/hr and
// exhausted the token (#1053's evidence). (The per-run branch bookkeeping the
// finding also flagged is a journal artifact, not API cost — deliberately out
// of scope here.)
//
// apiReadCache wraps the provider's HTTPClient seam (providers/http.go) with a
// disk-backed conditional-GET cache: on a GET it attaches If-None-Match from a
// stored ETag, and a GitHub 304 Not Modified — which does NOT count against the
// primary REST quota — is transparently replayed from the cached body. So an
// unchanged tick costs ~0 quota, and cost tracks change instead of backlog size.
//
// Correctness: a GitHub ETag is a content hash, so a 304 is GitHub asserting the
// body is byte-identical to what we cached — replaying it is zero-staleness, not
// "cached but possibly stale." The cache is also strictly fail-open: any lock,
// read, write, or corruption error falls through to the normal full GET, so it
// can never return wrong data or fail a request the network would have served.
//
// It mirrors the established cross-process cache discipline (#758 merge-policy,
// #523 sibling context): a single JSON file under the instance scheduler dir,
// guarded by withFileLock, written atomically. Sharing one store across the
// three stages also collapses their redundant independent PR listings (#1053
// mechanism #2) — the second stage this tick reuses the first's ETags.
const (
	apiReadCacheFileName   = "api-read-cache.json"
	apiReadCacheLockName   = "api-read-cache.lock"
	apiReadCacheTTL        = 7 * 24 * time.Hour
	apiReadCacheMaxEntries = 512
	// apiReadHTTPTimeout mirrors providers' own default provider HTTP timeout;
	// the wrapper's inner client keeps the same round-trip budget.
	apiReadHTTPTimeout = 60 * time.Second
)

// apiReadCacheEntry is one (token-scope, URL)'s cached conditional-GET result.
type apiReadCacheEntry struct {
	ETag   string `json:"etag"`
	Link   string `json:"link,omitempty"`        // replayed so pagination survives a 304
	Type   string `json:"contentType,omitempty"` // replayed Content-Type
	Body   []byte `json:"body"`                  // base64 in JSON
	Stored int64  `json:"storedAtUnix"`
}

func (e apiReadCacheEntry) storedAt() time.Time { return time.Unix(e.Stored, 0) }

func (e apiReadCacheEntry) fresh(now time.Time) bool {
	return now.Sub(e.storedAt()) <= apiReadCacheTTL
}

// response synthesizes the 200 the caller would have received, so provider
// send()/readPage()/readJSONResponse() consume it exactly as a live 200 — body
// plus the Link header pagination follows.
func (e apiReadCacheEntry) response(req *http.Request) *http.Response {
	h := http.Header{}
	if e.Link != "" {
		h.Set("Link", e.Link)
	}
	if e.Type != "" {
		h.Set("Content-Type", e.Type)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(e.Body)),
		Request:    req,
	}
}

// apiReadCache is a fail-open conditional-GET (ETag) HTTPClient decorator.
type apiReadCache struct {
	inner        providers.HTTPClient
	schedulerDir string

	mu     sync.Mutex
	mem    map[string]apiReadCacheEntry // loaded from disk once, then process-local
	loaded bool
}

// newAPIReadCache wraps inner with a conditional-GET cache backed by a JSON file
// under schedulerDir. A wrapper with an empty schedulerDir is a pass-through
// (standalone/manual invocation with no instance scheduler dir to persist into).
func newAPIReadCache(schedulerDir string, inner providers.HTTPClient) *apiReadCache {
	return &apiReadCache{inner: inner, schedulerDir: schedulerDir}
}

// apiReadCacheOption returns a provider option that routes GETs through the
// shared conditional-GET (ETag) cache under root's instance scheduler dir
// (#1053), wrapping a default HTTP client with providers' own timeout budget.
// The three read-heavy list stages (pr-select, gather-pr-context, backlog-query)
// each apply it so an unchanged tick's list GETs become zero-quota 304s and the
// stages share one ETag store.
func apiReadCacheOption(root string) func(*providers.GitHubProvider) {
	schedulerDir := layoutFor(root).SchedulerDir()
	inner := &http.Client{Timeout: apiReadHTTPTimeout}
	return providers.WithHTTPClient(newAPIReadCache(schedulerDir, inner))
}

// Do implements providers.HTTPClient. Only idempotent GETs are cached; every
// other method and any error path is a straight pass-through.
func (c *apiReadCache) Do(req *http.Request) (*http.Response, error) {
	if c == nil || c.schedulerDir == "" || req == nil || req.Method != http.MethodGet {
		return c.inner.Do(req)
	}

	key := apiReadCacheKey(req)
	entry, hit := c.lookup(key)
	if hit {
		req.Header.Set("If-None-Match", entry.ETag)
	}

	resp, err := c.inner.Do(req)
	if err != nil {
		return resp, err
	}

	// 304 is only reachable when we sent If-None-Match, i.e. we hold the body.
	if resp.StatusCode == http.StatusNotModified && hit {
		_ = resp.Body.Close()
		return entry.response(req), nil
	}

	// A fresh 200 carrying an ETag: buffer the body so we can both cache it and
	// hand an intact, re-readable response back to the caller.
	if resp.StatusCode == http.StatusOK {
		etag := resp.Header.Get("ETag")
		if etag == "" {
			return resp, nil
		}
		body, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			// The body is already partly consumed and unusable; surface the read
			// error the caller would have hit anyway.
			return nil, rerr
		}
		c.store(key, apiReadCacheEntry{
			ETag:   etag,
			Link:   resp.Header.Get("Link"),
			Type:   resp.Header.Get("Content-Type"),
			Body:   body,
			Stored: time.Now().Unix(),
		})
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return resp, nil
}

// apiReadCacheKey scopes an entry to its resource URL AND the credential's
// identity, via a non-reversible fingerprint of the Authorization header. Two
// stages on the same token (pr-select + gather-pr-context are both
// github:pr:write) share entries — collapsing their redundant PR listings — but
// a token with different read visibility can never replay another's body.
func apiReadCacheKey(req *http.Request) string {
	sum := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:8]) + "\x00" + req.URL.String()
}

// lookup returns a fresh cached entry for key, loading the disk cache into
// memory on first use. Fail-open: any load error yields an empty cache.
func (c *apiReadCache) lookup(key string) (apiReadCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.mem = c.readDisk()
		c.loaded = true
	}
	entry, ok := c.mem[key]
	if !ok || !entry.fresh(time.Now()) {
		return apiReadCacheEntry{}, false
	}
	return entry, true
}

// store records entry in memory and persists it. Persistence happens only on a
// changed resource (a 200 with a new ETag) — an all-304 tick writes nothing, so
// disk I/O tracks change, not tick count. A persist failure is swallowed
// (fail-open): the in-memory copy still serves the rest of this process.
func (c *apiReadCache) store(key string, entry apiReadCacheEntry) {
	c.mu.Lock()
	if c.mem == nil {
		c.mem = map[string]apiReadCacheEntry{}
	}
	c.mem[key] = entry
	c.mu.Unlock()

	lockPath := filepath.Join(c.schedulerDir, apiReadCacheLockName)
	_ = withFileLock(lockPath, func() error {
		onDisk := c.readDisk() // re-read under lock so we merge, not clobber, a peer's writes
		onDisk[key] = entry
		return c.writeDisk(evictAPIReadCache(onDisk))
	})
}

// readDisk loads the cache file, dropping stale entries. Any error (missing
// file, unreadable, corrupt JSON) returns an empty map — never fails a caller.
// Atomic writes make an unlocked read safe: a reader sees the whole old or whole
// new file, never a torn one.
func (c *apiReadCache) readDisk() map[string]apiReadCacheEntry {
	out := map[string]apiReadCacheEntry{}
	data, err := os.ReadFile(filepath.Join(c.schedulerDir, apiReadCacheFileName))
	if err != nil {
		return out
	}
	var file struct {
		Entries map[string]apiReadCacheEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return out
	}
	now := time.Now()
	for k, e := range file.Entries {
		if e.fresh(now) {
			out[k] = e
		}
	}
	return out
}

// writeDisk persists entries atomically (temp + rename) so a concurrent reader
// or a crash mid-write never observes a partial file.
func (c *apiReadCache) writeDisk(entries map[string]apiReadCacheEntry) error {
	data, err := json.Marshal(struct {
		Entries map[string]apiReadCacheEntry `json:"entries"`
	}{Entries: entries})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.schedulerDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.schedulerDir, "."+apiReadCacheFileName+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(c.schedulerDir, apiReadCacheFileName))
}

// evictAPIReadCache bounds the file: it drops entries beyond apiReadCacheMaxEntries,
// evicting the oldest (by stored time) first so hot, recently-refreshed entries
// survive. Returns the same map for call-site convenience.
func evictAPIReadCache(entries map[string]apiReadCacheEntry) map[string]apiReadCacheEntry {
	if len(entries) <= apiReadCacheMaxEntries {
		return entries
	}
	type keyed struct {
		key    string
		stored int64
	}
	all := make([]keyed, 0, len(entries))
	for k, e := range entries {
		all = append(all, keyed{key: k, stored: e.Stored})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].stored > all[j].stored })
	for _, k := range all[apiReadCacheMaxEntries:] {
		delete(entries, k.key)
	}
	return entries
}
