// Package apireadcache is the baseline GitHub API READ-volume reduction
// (issue #1053).
//
// The daemon's workflow stages repeatedly consume the same open-PR and backlog
// lists during one scheduler evaluation. Re-fetching those collections made
// primary-REST-quota cost scale with consumer count and backlog size rather than
// with what changed since the prior tick.
//
// apiReadCache wraps the provider's HTTPClient seam (providers/http.go) with a
// disk-backed conditional-GET cache: on a GET it attaches If-None-Match from a
// stored strong ETag, and a GitHub 304 Not Modified — which does NOT count
// against the primary REST quota — is transparently replayed from the cached
// body. For endpoints with strong validators, an unchanged tick costs ~0 quota,
// and cost tracks change instead of backlog size.
//
// Correctness: only a strong ETag can validate byte-equivalent content. GitHub's
// weak ETags are persisted but never sent in conditional requests because weak
// validators on label-filtered issue collections can remain unchanged when
// membership changes. Last-Modified is retained as the fallback for endpoints
// without ETags. The cache is also strictly fail-open: any lock, read, write, or
// corruption error falls through to the normal full GET.
//
// It mirrors the established cross-process cache discipline (#758 merge-policy,
// #523 sibling context): a shared store under the instance scheduler dir,
// guarded by a bounded file lock. Indexed SQLite updates persist only the changed
// entry and incrementally maintain body references. Sharing one store across
// the list consumers also collapses their redundant independent listings —
// later stages in the scheduler evaluation reuse the first stage's snapshot.
package apireadcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/apireadstore"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/providers"
)

const (
	apiReadCacheLockName   = "api-read-cache.lock"
	apiReadCacheTTL        = 7 * 24 * time.Hour
	apiReadSnapshotTTL     = time.Hour
	apiReadCacheMaxEntries = 512
	apiReadCacheMaxBytes   = 16 << 20
	// apiReadHTTPTimeout mirrors providers' own default provider HTTP timeout;
	// the wrapper's inner client keeps the same round-trip budget.
	apiReadHTTPTimeout              = 60 * time.Second
	apiReadCacheLockAcquireTimeout  = time.Second
	apiReadCacheLockRetryInterval   = 10 * time.Millisecond
	apiReadCacheMaxLockAcquisitions = 4
)

var apiReadCacheLocks = newAPIReadCacheLockManager(apiReadCacheMaxLockAcquisitions)

// apiReadCacheEntry is one (token-scope, URL)'s cached conditional-GET result.
type apiReadCacheEntry struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Link         string `json:"link,omitempty"`        // replayed so pagination survives a 304
	Type         string `json:"contentType,omitempty"` // replayed Content-Type
	Body         []byte `json:"body,omitempty"`        // response body; persisted separately from metadata
	BodyRef      string `json:"bodyRef,omitempty"`
	Stored       int64  `json:"storedAtUnix"`
	Snapshot     string `json:"snapshot,omitempty"`
}

func (e apiReadCacheEntry) storedAt() time.Time { return time.Unix(e.Stored, 0) }

func (e apiReadCacheEntry) fresh(now time.Time) bool {
	ttl := apiReadCacheTTL
	if e.Snapshot != "" {
		ttl = apiReadSnapshotTTL
	}
	return now.Sub(e.storedAt()) <= ttl
}

// response synthesizes the 200 the caller would have received, so provider
// send()/readPage()/readJSONResponse() consume it exactly as a live 200 — body
// plus the Link header pagination follows.
func (e apiReadCacheEntry) response(req *http.Request) *http.Response {
	h := http.Header{}
	h.Set(providers.QuotaCacheHitHeader, "true")
	if e.Link != "" {
		h.Set("Link", e.Link)
	}
	if e.Type != "" {
		h.Set("Content-Type", e.Type)
	}
	if e.ETag != "" {
		h.Set("ETag", e.ETag)
	}
	if e.LastModified != "" {
		h.Set("Last-Modified", e.LastModified)
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
	snapshotID   string
	quotaGate    providers.QuotaRequestGate

	mu  sync.Mutex
	mem map[string]apiReadCacheEntry // bounded process-local responses
}

// newAPIReadCache wraps inner with a conditional-GET cache backed by SQLite
// under schedulerDir. snapshotID coalesces provider list reads started by the
// same scheduler evaluation. A wrapper with an empty schedulerDir is a
// pass-through (standalone/manual invocation with no instance scheduler dir to
// persist into).
func newAPIReadCache(schedulerDir, snapshotID string, inner providers.HTTPClient) *apiReadCache {
	CleanStaleLocks(schedulerDir)
	return &apiReadCache{inner: inner, schedulerDir: schedulerDir, snapshotID: snapshotID}
}

// apiReadCacheStaleLockAge is how old an api-read-cache per-list-key lock
// file (apiReadListLockPath) must be before startup cleanup treats it as
// debris rather than a lock a peer might still be contending for.
// lock.Acquire creates its file with O_CREATE but Release only unlocks and
// closes it (internal/platform/lock) — by design, nothing ever unlinks it —
// so every distinct list-request key this scheduler dir has ever seen leaves
// a permanent zero-byte file behind. No withFileLock critical section here
// runs anywhere close to this long, so a file this old is safe to remove.
const apiReadCacheStaleLockAge = 24 * time.Hour

// CleanStaleLocks removes apiReadListLockPath lock files under schedulerDir
// older than apiReadCacheStaleLockAge. Before removing one it takes a non-blocking
// lock on it, which both confirms no peer currently holds it and closes the
// TOCTOU window between the age check and the removal — a peer that opens
// the path afterward simply creates a fresh file and locks that instead.
// Best effort throughout: any error just leaves the file for a later sweep,
// and this must never fail cache construction.
//
// Runs unconditionally on every call — deliberately NOT gated by a
// once-per-schedulerDir guard (#4251). lock.Release never unlinks its file by
// design (see the doc above), so every distinct list-request key a scheduler
// dir has ever seen leaves a permanent zero-byte lock file behind; gating
// this sweep to run only once per process meant a long-lived daemon's own
// construction calls (each cache-construction call site — stage dispatch,
// open-PR polling, counter evaluation — recurs throughout its lifetime) never
// got a second chance to reclaim them, so locks accrued unbounded between
// restarts (measured ~14/hour). The scan itself stays cheap and self-limiting
// regardless of call frequency: it's one os.ReadDir plus a mod-time compare
// per entry, and only files already past the 24h cutoff are ever touched, so
// calling it often costs nothing extra on a directory with nothing stale.
// The daemon (cmd/goobers runUpContext) also drives it from a periodic sweep
// ticker so a daemon whose own construction calls happen to be infrequent (or
// absent, e.g. no open-PR polling configured) still gets a periodic pass.
func CleanStaleLocks(schedulerDir string) {
	if schedulerDir == "" {
		return
	}
	entries, err := os.ReadDir(schedulerDir)
	if err != nil {
		return
	}
	prefix := apiReadCacheLockName + "."
	cutoff := time.Now().Add(-apiReadCacheStaleLockAge)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(schedulerDir, name)
		held, err := tryAcquireAPIReadCacheLock(path, apiReadCacheLockAcquireTimeout, lock.TryAcquire)
		if err != nil {
			continue // a live peer holds it (or it's otherwise unavailable) — leave it
		}
		_ = os.Remove(path)
		_ = held.Release()
	}
}

func (c *apiReadCache) SetQuotaRequestGate(gate providers.QuotaRequestGate) {
	c.quotaGate = gate
}

// Option returns a provider option that routes GETs through the shared
// conditional-GET (ETag) cache under schedulerDir (#1053), wrapping a default
// HTTP client with providers' own timeout budget. Provider list consumers apply
// it so strongly validated unchanged GETs become zero-quota 304s and all stages
// share one response store. snapshotID coalesces provider list reads started by
// the same scheduler evaluation; empty disables snapshot coalescing.
func Option(schedulerDir, snapshotID string) func(*providers.GitHubProvider) {
	inner := &http.Client{Timeout: apiReadHTTPTimeout}
	return providers.WithHTTPClient(newAPIReadCache(schedulerDir, snapshotID, inner))
}

// InvalidateSnapshot drops every cached list response recorded under
// snapshotID in schedulerDir's store. An empty snapshotID is a no-op.
func InvalidateSnapshot(schedulerDir, snapshotID string) error {
	if snapshotID == "" {
		return nil
	}
	cache := newAPIReadCache(schedulerDir, snapshotID, nil)
	return cache.withDisk(func(store *apireadstore.Store) error { return store.InvalidateSnapshot(snapshotID) })
}

// Do implements providers.HTTPClient. Only idempotent GETs are cached; every
// other method and any error path is a straight pass-through.
func (c *apiReadCache) Do(req *http.Request) (*http.Response, error) {
	if c == nil || c.schedulerDir == "" || req == nil || req.Method != http.MethodGet {
		return c.do(req)
	}

	key := apiReadCacheKey(req)
	if c.snapshotID != "" && isProviderListRequest(req) {
		snapshotKey := apiReadSnapshotKey(c.snapshotID, key)
		if entry, hit := c.lookup(snapshotKey); hit {
			return entry.response(req), nil
		}
		var (
			resp       *http.Response
			requestErr error
		)
		lockErr := withAPIReadCacheLock(apiReadListLockPath(c.schedulerDir, key), func() error {
			if entry, hit := c.lookupDisk(snapshotKey); hit {
				resp = entry.response(req)
				return nil
			}
			entry, hit := c.lookupDisk(key)
			resp, requestErr = c.fetch(req, entry, hit, true, func(updated apiReadCacheEntry) {
				updated.Stored = time.Now().Unix()
				updated.Snapshot = ""
				snapshot := updated
				snapshot.Snapshot = c.snapshotID
				c.remember(key, updated)
				c.remember(snapshotKey, snapshot)
				c.persist(map[string]apiReadCacheEntry{key: updated, snapshotKey: snapshot})
			})
			return nil
		})
		if lockErr == nil {
			return resp, requestErr
		}
	}

	entry, hit := c.lookup(key)
	return c.fetch(req, entry, hit, false, func(updated apiReadCacheEntry) {
		c.store(key, updated)
	})
}

func (c *apiReadCache) fetch(req *http.Request, entry apiReadCacheEntry, hit, snapshot bool, save func(apiReadCacheEntry)) (*http.Response, error) {
	validatorSent := false
	if hit {
		switch {
		case isStrongETag(entry.ETag):
			req.Header.Set("If-None-Match", entry.ETag)
			validatorSent = true
		case entry.ETag == "" && entry.LastModified != "":
			req.Header.Set("If-Modified-Since", entry.LastModified)
			validatorSent = true
		}
	}
	resp, err := c.do(req)
	if err != nil {
		return resp, err
	}

	// 304 is only replayable when this cache sent a trustworthy validator and
	// still holds the corresponding body.
	if resp.StatusCode == http.StatusNotModified && hit && validatorSent {
		_ = resp.Body.Close()
		validatorChanged := false
		if etag := resp.Header.Get("ETag"); etag != "" {
			validatorChanged = etag != entry.ETag
			entry.ETag = etag
		}
		if modified := resp.Header.Get("Last-Modified"); modified != "" {
			validatorChanged = validatorChanged || modified != entry.LastModified
			entry.LastModified = modified
		}
		if snapshot || validatorChanged {
			save(entry)
		}
		return entry.response(req), nil
	}

	// A fresh 200 carrying a validator (or belonging to a scheduler snapshot):
	// buffer the body so we can cache it and hand an intact response to the caller.
	if resp.StatusCode == http.StatusOK {
		etag := resp.Header.Get("ETag")
		modified := resp.Header.Get("Last-Modified")
		if etag == "" && modified == "" && !snapshot {
			return resp, nil
		}
		body, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			// The body is already partly consumed and unusable; surface the read
			// error the caller would have hit anyway.
			return nil, rerr
		}
		save(apiReadCacheEntry{
			ETag:         etag,
			LastModified: modified,
			Link:         resp.Header.Get("Link"),
			Type:         resp.Header.Get("Content-Type"),
			Body:         body,
			Stored:       time.Now().Unix(),
		})
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return resp, nil
}

func isStrongETag(etag string) bool {
	etag = strings.TrimSpace(etag)
	return etag != "" && !strings.HasPrefix(etag, "W/")
}

func (c *apiReadCache) do(req *http.Request) (*http.Response, error) {
	if c.quotaGate != nil {
		if err := c.quotaGate.AcquireQuotaRequest(req.Context(), providers.ProviderGitHub); err != nil {
			return nil, err
		}
	}
	return c.inner.Do(req)
}

func isProviderListRequest(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	for i, part := range parts {
		if part != "repos" || len(parts) != i+4 {
			continue
		}
		resource := parts[len(parts)-1]
		return resource == "pulls" || resource == "issues"
	}
	return false
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

func apiReadSnapshotKey(snapshotID, key string) string {
	return "snapshot\x00" + snapshotID + "\x00" + key
}

func apiReadListLockPath(schedulerDir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(schedulerDir, apiReadCacheLockName+"."+hex.EncodeToString(sum[:8]))
}

type apiReadCacheLockResult struct {
	handle *lock.Handle
	err    error
}

type apiReadCacheLockAttempt struct {
	done chan struct{}

	mu        sync.Mutex
	result    apiReadCacheLockResult
	completed bool
	abandoned bool
}

// apiReadCacheLockManager deduplicates blocked opens by path and caps blocked
// OS calls across distinct cache keys so a daemon cannot leak resources without
// bound when CreateFile never returns.
type apiReadCacheLockManager struct {
	mu       sync.Mutex
	inFlight map[string]*apiReadCacheLockAttempt
	slots    chan struct{}
}

func newAPIReadCacheLockManager(limit int) *apiReadCacheLockManager {
	return &apiReadCacheLockManager{
		inFlight: make(map[string]*apiReadCacheLockAttempt),
		slots:    make(chan struct{}, limit),
	}
}

// acquireAPIReadCacheLock bounds the file open that precedes LockFileEx and can
// otherwise block indefinitely inside CreateFile on Windows.
func acquireAPIReadCacheLock(lockPath string, timeout time.Duration, acquire func(string) (*lock.Handle, error)) (*lock.Handle, error) {
	return apiReadCacheLocks.acquire(lockPath, timeout, acquire)
}

func (m *apiReadCacheLockManager) acquire(lockPath string, timeout time.Duration, acquire func(string) (*lock.Handle, error)) (*lock.Handle, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("api read cache lock %q: acquisition timed out after %s", lockPath, timeout)
		}
		attempt, owner, err := m.start(lockPath, acquire)
		if err != nil {
			return nil, err
		}

		timer := time.NewTimer(remaining)
		select {
		case <-attempt.done:
			timer.Stop()
			if !owner {
				continue
			}
		case <-timer.C:
			if owner {
				attempt.abandon()
			}
			return nil, fmt.Errorf("api read cache lock %q: acquisition timed out after %s", lockPath, timeout)
		}

		handle, err := attempt.acquired()
		if !errors.Is(err, lock.ErrHeld) {
			return handle, err
		}

		delay := min(apiReadCacheLockRetryInterval, time.Until(deadline))
		if delay <= 0 {
			return nil, fmt.Errorf("api read cache lock %q: acquisition timed out after %s", lockPath, timeout)
		}
		time.Sleep(delay)
	}
}

func (m *apiReadCacheLockManager) start(lockPath string, acquire func(string) (*lock.Handle, error)) (*apiReadCacheLockAttempt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if attempt := m.inFlight[lockPath]; attempt != nil {
		return attempt, false, nil
	}
	select {
	case m.slots <- struct{}{}:
	default:
		return nil, false, fmt.Errorf("api read cache lock %q: acquisition capacity exhausted", lockPath)
	}

	attempt := &apiReadCacheLockAttempt{done: make(chan struct{})}
	m.inFlight[lockPath] = attempt
	go func() {
		handle, err := acquire(lockPath)
		attempt.mu.Lock()
		attempt.completed = true
		if attempt.abandoned {
			_ = handle.Release()
		} else {
			attempt.result = apiReadCacheLockResult{handle: handle, err: err}
		}
		attempt.mu.Unlock()

		m.mu.Lock()
		delete(m.inFlight, lockPath)
		<-m.slots
		m.mu.Unlock()
		close(attempt.done)
	}()
	return attempt, true, nil
}

func (a *apiReadCacheLockAttempt) abandon() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.abandoned = true
	if a.completed {
		_ = a.result.handle.Release()
		a.result.handle = nil
	}
}

func (a *apiReadCacheLockAttempt) acquired() (*lock.Handle, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result.handle, a.result.err
}

func tryAcquireAPIReadCacheLock(lockPath string, timeout time.Duration, acquire func(string) (*lock.Handle, error)) (*lock.Handle, error) {
	return apiReadCacheLocks.acquire(lockPath, timeout, acquire)
}

// withAPIReadCacheLock fails open on contention or a blocked file open. The
// cache is optional, so provider reads must not wait behind its filesystem I/O.
func withAPIReadCacheLock(lockPath string, fn func() error) error {
	held, err := acquireAPIReadCacheLock(lockPath, apiReadCacheLockAcquireTimeout, lock.TryAcquire)
	if err != nil {
		return err
	}
	defer func() { _ = held.Release() }()
	return fn()
}

// lookup loads only the requested response. Memory is bounded independently of
// the shared cache; a missing/expired body simply falls through to a full GET.
func (c *apiReadCache) lookup(key string) (apiReadCacheEntry, bool) {
	c.mu.Lock()
	entry, ok := c.mem[key]
	c.mu.Unlock()
	if ok && entry.fresh(time.Now()) {
		return entry, true
	}
	entry, ok = c.lookupDisk(key)
	if ok {
		c.remember(key, entry)
	}
	return entry, ok
}

func (c *apiReadCache) remember(key string, entry apiReadCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mem == nil {
		c.mem = map[string]apiReadCacheEntry{}
	}
	if _, exists := c.mem[key]; !exists && len(c.mem) >= apiReadCacheMaxEntries {
		for old := range c.mem {
			delete(c.mem, old)
			break
		}
	}
	c.mem[key] = entry
}

func (c *apiReadCache) store(key string, entry apiReadCacheEntry) {
	c.remember(key, entry)
	c.persist(map[string]apiReadCacheEntry{key: entry})
}

func (c *apiReadCache) withDisk(fn func(*apireadstore.Store) error) error {
	return withAPIReadCacheLock(filepath.Join(c.schedulerDir, apiReadCacheLockName), func() error {
		store, err := apireadstore.Open(c.schedulerDir, apiReadCacheMaxEntries, apiReadCacheMaxBytes)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		return fn(store)
	})
}

func (c *apiReadCache) lookupDisk(key string) (apiReadCacheEntry, bool) {
	var entry apiReadCacheEntry
	hit := false
	_ = c.withDisk(func(store *apireadstore.Store) error {
		saved, ok, err := store.Get(key, time.Now())
		if err != nil || !ok {
			return err
		}
		if err := json.Unmarshal(saved.Metadata, &entry); err != nil {
			return err
		}
		entry.Body = saved.Body
		hit = true
		return nil
	})
	return entry, hit
}

func (c *apiReadCache) persist(entries map[string]apiReadCacheEntry) {
	saved := make([]apireadstore.Entry, 0, len(entries))
	for key, entry := range entries {
		body := entry.Body
		entry.Body, entry.BodyRef = nil, ""
		metadata, err := json.Marshal(entry)
		if err != nil {
			return
		}
		ttl := apiReadCacheTTL
		if entry.Snapshot != "" {
			ttl = apiReadSnapshotTTL
		}
		saved = append(saved, apireadstore.Entry{Key: key, Metadata: metadata, Body: body,
			Stored: entry.Stored, Expires: entry.Stored + int64(ttl/time.Second), Snapshot: entry.Snapshot})
	}
	_ = c.withDisk(func(store *apireadstore.Store) error { return store.Put(saved...) })
}
