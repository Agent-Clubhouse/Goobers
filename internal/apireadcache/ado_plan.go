package apireadcache

import (
	"crypto/rand"
	"net/http"
	"sync/atomic"

	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/providers"
)

// Optional sharing may wait for at most this many caller-owned reads. Excess
// callers use the normal transport; the cache never starts background requests.
var adoReadWaiters = make(chan struct{}, 64)

// A process-local monotonic fence cannot replay an old response after a clock
// adjustment or restart. Explicit evaluation snapshots also share across processes.
var adoReadProcess = rand.Text()
var adoReadSequence atomic.Uint64

func (c *apiReadCache) readADOPlan(req *http.Request, digest string) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if !scopedCacheableHeaders(req) || req.Header.Get("Authorization") == "" {
		return c.do(req)
	}
	select {
	case adoReadWaiters <- struct{}{}:
		defer func() { <-adoReadWaiters }()
	default:
		return c.do(req)
	}
	started := adoReadSequence.Add(1)
	requestKey, err := c.scopedReadKey(req)
	if err != nil {
		return c.do(req)
	}
	key := "ado-list-plan-v1:" + digest + ":" + requestKey
	if c.snapshotID != "" {
		var err error
		key, err = c.snapshotReadKey(key)
		if err != nil {
			return c.do(req)
		}
	}
	held, err := apiReadCacheLocks.acquireContext(req.Context(), apiReadListLockPath(c.schedulerDir, key), c.lockBudget, lock.TryAcquire)
	if err != nil {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		return c.do(req)
	}
	defer func() { _ = held.Release() }()
	// Always consult disk: explicit invalidation must also affect a reused client.
	entry, hit := c.lookupDisk(key)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if hit && (c.snapshotID != "" || entry.ReadProcess == adoReadProcess && entry.CompletedSequence > started) {
		return entry.response(req), nil
	}
	return c.fetchADOPlan(req, key)
}

func (c *apiReadCache) fetchADOPlan(req *http.Request, key string) (*http.Response, error) {
	response, err := c.do(req)
	if err != nil {
		return response, err
	}
	if err := req.Context().Err(); err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return response, nil
	}
	// ADO POST reads have no conditional replay contract. Store only whole 200
	// responses; no errors, 429s, validators, per-item merging or negative fills.
	response, err = cacheReadResponse(response, true, func(entry apiReadCacheEntry) {
		if req.Context().Err() != nil || !providers.ValidADOReadPlanResponse(req, entry.Body) {
			return
		}
		entry.ReadProcess = adoReadProcess
		entry.CompletedSequence = adoReadSequence.Add(1)
		entry.Snapshot = c.snapshotID
		c.persist(map[string]apiReadCacheEntry{key: entry})
	})
	if err != nil {
		return response, err
	}
	if err := req.Context().Err(); err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	return response, nil
}
