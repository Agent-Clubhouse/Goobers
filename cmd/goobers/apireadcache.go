package main

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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

// Baseline GitHub API READ-volume reduction (issue #1053).
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
// #523 sibling context): a single JSON file under the instance scheduler dir,
// guarded by a bounded file lock, written atomically. Sharing one store across
// the list consumers also collapses their redundant independent listings —
// later stages in the scheduler evaluation reuse the first stage's snapshot.
const (
	apiReadCacheFileName   = "api-read-cache.json"
	apiReadCacheBodyDir    = "api-read-cache-bodies"
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
	Body         []byte `json:"body,omitempty"`        // legacy inline body; new writes use BodyRef
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

	mu     sync.Mutex
	mem    map[string]apiReadCacheEntry // loaded from disk once, then process-local
	loaded bool
}

// newAPIReadCache wraps inner with a conditional-GET cache backed by a JSON file
// under schedulerDir. snapshotID coalesces provider list reads started by the
// same scheduler evaluation. A wrapper with an empty schedulerDir is a
// pass-through (standalone/manual invocation with no instance scheduler dir to
// persist into).
func newAPIReadCache(schedulerDir, snapshotID string, inner providers.HTTPClient) *apiReadCache {
	cleanStaleAPIReadCacheLocks(schedulerDir)
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

// apiReadCacheLockCleanupDone tracks which scheduler dirs have already had
// their stale per-list-key locks swept in this process, so a long-lived
// daemon does the directory scan once at startup rather than on every cache
// construction.
var apiReadCacheLockCleanupDone sync.Map // schedulerDir -> *sync.Once

// cleanStaleAPIReadCacheLocks removes apiReadListLockPath lock files older
// than apiReadCacheStaleLockAge. Before removing one it takes a non-blocking
// lock on it, which both confirms no peer currently holds it and closes the
// TOCTOU window between the age check and the removal — a peer that opens
// the path afterward simply creates a fresh file and locks that instead.
// Best effort throughout: any error just leaves the file for a later sweep,
// and this must never fail cache construction.
func cleanStaleAPIReadCacheLocks(schedulerDir string) {
	if schedulerDir == "" {
		return
	}
	onceVal, _ := apiReadCacheLockCleanupDone.LoadOrStore(schedulerDir, new(sync.Once))
	onceVal.(*sync.Once).Do(func() {
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
	})
}

func (c *apiReadCache) SetQuotaRequestGate(gate providers.QuotaRequestGate) {
	c.quotaGate = gate
}

// apiReadCacheOption returns a provider option that routes GETs through the
// shared conditional-GET (ETag) cache under root's instance scheduler dir
// (#1053), wrapping a default HTTP client with providers' own timeout budget.
// Provider list consumers apply it so strongly validated unchanged GETs become
// zero-quota 304s and all stages share one response store.
func apiReadCacheOption(root string) func(*providers.GitHubProvider) {
	return apiReadCacheOptionForSnapshot(layoutFor(root).SchedulerDir(), os.Getenv(providersnapshot.EnvVar))
}

func apiReadCacheOptionForSnapshot(schedulerDir, snapshotID string) func(*providers.GitHubProvider) {
	inner := &http.Client{Timeout: apiReadHTTPTimeout}
	return providers.WithHTTPClient(newAPIReadCache(schedulerDir, snapshotID, inner))
}

func newCachedGitHubProvider(root, token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
	return newGitHubProvider(token, append(opts, apiReadCacheOption(root))...)
}

func invalidateCurrentProviderSnapshot(root string) error {
	snapshotID := os.Getenv(providersnapshot.EnvVar)
	if snapshotID == "" {
		return nil
	}
	schedulerDir := layoutFor(root).SchedulerDir()
	cache := newAPIReadCache(schedulerDir, snapshotID, nil)
	return withAPIReadCacheLock(filepath.Join(schedulerDir, apiReadCacheLockName), func() error {
		entries := cache.readDiskUnlocked()
		prefix := "snapshot\x00" + snapshotID + "\x00"
		for key := range entries {
			if strings.HasPrefix(key, prefix) {
				delete(entries, key)
			}
		}
		return cache.writeDisk(evictAPIReadCache(entries))
	})
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
			entries := c.readDisk()
			c.replaceMemory(entries)
			if entry, hit := entries[snapshotKey]; hit {
				resp = entry.response(req)
				return nil
			}
			entry, hit := entries[key]
			resp, requestErr = c.fetch(req, entry, hit, true, func(updated apiReadCacheEntry) {
				updated.Stored = time.Now().Unix()
				updated.Snapshot = ""
				snapshot := updated
				snapshot.Snapshot = c.snapshotID
				c.remember(key, updated)
				c.remember(snapshotKey, snapshot)
				_ = withAPIReadCacheLock(filepath.Join(c.schedulerDir, apiReadCacheLockName), func() error {
					onDisk := c.readDiskUnlocked()
					onDisk[key] = updated
					onDisk[snapshotKey] = snapshot
					return c.writeDisk(evictAPIReadCache(onDisk))
				})
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

func (c *apiReadCache) remember(key string, entry apiReadCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mem == nil {
		c.mem = map[string]apiReadCacheEntry{}
	}
	c.mem[key] = entry
	c.loaded = true
}

func (c *apiReadCache) replaceMemory(entries map[string]apiReadCacheEntry) {
	c.mu.Lock()
	c.mem = make(map[string]apiReadCacheEntry, len(entries))
	for key, entry := range entries {
		c.mem[key] = entry
	}
	c.loaded = true
	c.mu.Unlock()
}

// store records entry in memory and persists it. A persist failure is swallowed
// (fail-open): the in-memory copy still serves the rest of this process.
func (c *apiReadCache) store(key string, entry apiReadCacheEntry) {
	c.mu.Lock()
	if c.mem == nil {
		c.mem = map[string]apiReadCacheEntry{}
	}
	c.mem[key] = entry
	c.mu.Unlock()

	lockPath := filepath.Join(c.schedulerDir, apiReadCacheLockName)
	_ = withAPIReadCacheLock(lockPath, func() error {
		onDisk := c.readDiskUnlocked() // re-read under lock so we merge, not clobber, a peer's writes
		onDisk[key] = entry
		return c.writeDisk(evictAPIReadCache(onDisk))
	})
}

// readDisk loads the cache file, dropping stale entries. Any error (missing
// file, unreadable, corrupt JSON) returns an empty map — never fails a caller.
func (c *apiReadCache) readDisk() map[string]apiReadCacheEntry {
	out := map[string]apiReadCacheEntry{}
	if err := withAPIReadCacheLock(filepath.Join(c.schedulerDir, apiReadCacheLockName), func() error {
		out = c.readDiskUnlocked()
		return nil
	}); err != nil {
		return map[string]apiReadCacheEntry{}
	}
	return out
}

func (c *apiReadCache) readDiskUnlocked() map[string]apiReadCacheEntry {
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
		if !e.fresh(now) {
			continue
		}
		if e.BodyRef != "" {
			body, err := os.ReadFile(filepath.Join(c.schedulerDir, apiReadCacheBodyDir, e.BodyRef))
			if err != nil || apiReadBodyRef(body) != e.BodyRef {
				continue
			}
			e.Body = body
		} else if e.Body == nil {
			continue
		}
		out[k] = e
	}
	return out
}

// writeDisk persists small metadata atomically and response bodies once under
// content-addressed names. Snapshot aliases therefore do not duplicate or
// repeatedly rewrite full provider responses.
func (c *apiReadCache) writeDisk(entries map[string]apiReadCacheEntry) error {
	entries = evictAPIReadCache(entries)
	if err := os.MkdirAll(filepath.Join(c.schedulerDir, apiReadCacheBodyDir), 0o755); err != nil {
		return err
	}
	persisted := make(map[string]apiReadCacheEntry, len(entries))
	bodyRefs := make(map[string]bool, len(entries))
	for key, entry := range entries {
		ref := apiReadBodyRef(entry.Body)
		if err := c.writeBody(ref, entry.Body); err != nil {
			return err
		}
		entry.Body = nil
		entry.BodyRef = ref
		persisted[key] = entry
		bodyRefs[ref] = true
	}
	data, err := json.Marshal(struct {
		Entries map[string]apiReadCacheEntry `json:"entries"`
	}{Entries: persisted})
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
	if err := os.Rename(tmpName, filepath.Join(c.schedulerDir, apiReadCacheFileName)); err != nil {
		return err
	}
	c.removeUnreferencedBodies(bodyRefs)
	return nil
}

func apiReadBodyRef(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (c *apiReadCache) writeBody(ref string, body []byte) error {
	path := filepath.Join(c.schedulerDir, apiReadCacheBodyDir, ref)
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(body)) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+ref+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func (c *apiReadCache) removeUnreferencedBodies(referenced map[string]bool) {
	dir := filepath.Join(c.schedulerDir, apiReadCacheBodyDir)
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, file := range files {
		if !referenced[file.Name()] {
			_ = os.Remove(filepath.Join(dir, file.Name()))
		}
	}
}

// evictAPIReadCache bounds metadata count and unique persisted response bytes,
// retaining newest base entries before same-tick snapshot aliases.
func evictAPIReadCache(entries map[string]apiReadCacheEntry) map[string]apiReadCacheEntry {
	return evictAPIReadCacheToLimits(entries, apiReadCacheMaxEntries, apiReadCacheMaxBytes)
}

func evictAPIReadCacheToLimits(entries map[string]apiReadCacheEntry, maxEntries, maxBytes int) map[string]apiReadCacheEntry {
	type keyed struct {
		key   string
		entry apiReadCacheEntry
	}
	all := make([]keyed, 0, len(entries))
	for k, e := range entries {
		all = append(all, keyed{key: k, entry: e})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].entry.Stored != all[j].entry.Stored {
			return all[i].entry.Stored > all[j].entry.Stored
		}
		if (all[i].entry.Snapshot == "") != (all[j].entry.Snapshot == "") {
			return all[i].entry.Snapshot == ""
		}
		return all[i].key < all[j].key
	})
	kept := make(map[string]apiReadCacheEntry, min(len(entries), maxEntries))
	refs := make(map[string]bool)
	bodyBytes := 0
	for _, item := range all {
		if len(kept) == maxEntries {
			break
		}
		ref := apiReadBodyRef(item.entry.Body)
		addedBytes := 0
		if !refs[ref] {
			addedBytes = len(item.entry.Body)
		}
		if bodyBytes+addedBytes > maxBytes {
			continue
		}
		kept[item.key] = item.entry
		refs[ref] = true
		bodyBytes += addedBytes
	}
	return kept
}
