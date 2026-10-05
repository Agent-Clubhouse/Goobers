package apireadcache

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/goobers/goobers/internal/platform/lock"
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
	key := "ado-list-plan-v1:" + digest + ":" + scopedRequestKey(c.partition, req)
	if c.snapshotID != "" {
		key = apiReadSnapshotKey(c.snapshotID, key)
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
		if req.Context().Err() != nil || !validADOPlanResponse(req, entry.Body) {
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

func validADOPlanResponse(req *http.Request, body []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	field := "value"
	if strings.HasSuffix(req.URL.Path, "/wiql") {
		field = "workItems"
	}
	raw := strings.TrimSpace(string(envelope[field]))
	if !strings.HasPrefix(raw, "[") {
		return false
	}
	// A syntactically valid array can still fail the provider's typed decoder.
	// Null batch entries represent omissions and remain valid; scalar members or
	// incompatible IDs must never become a replayable successful response.
	var rows []*struct {
		ID        int                        `json:"id"`
		URL       string                     `json:"url"`
		Rev       int                        `json:"rev"`
		Fields    map[string]json.RawMessage `json:"fields"`
		Relations []json.RawMessage          `json:"relations"`
	}
	return json.Unmarshal([]byte(raw), &rows) == nil
}
