package apireadcache

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/providers"
)

func (c *apiReadCache) adoStateMetadataRequest(req *http.Request) bool {
	if c.partition == "" || c.provider != providers.ProviderADO || req.Header.Get("Authorization") == "" {
		return false
	}
	_, suffix, ok := strings.Cut(req.URL.EscapedPath(), "/_apis/wit/workitemtypes/")
	parts := strings.Split(suffix, "/")
	return ok && len(parts) == 2 && parts[0] != "" && parts[1] == "states"
}

func (c *apiReadCache) readADOStateMetadata(req *http.Request) (*http.Response, error) {
	// PR and work-item mutations do not change process definitions. Keep the
	// authorization partition and full representation/credential identity, but
	// do not invalidate metadata on every ordinary stage write.
	key := "ado-state-metadata-v1:" + scopedRequestKey(c.partition, req)
	held, err := apiReadCacheLocks.acquireContext(req.Context(), apiReadListLockPath(c.schedulerDir, key), c.lockBudget, lock.TryAcquire)
	if err != nil {
		return c.do(req)
	}
	defer func() { _ = held.Release() }()
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	entry, hit := c.lookupDisk(key)
	if hit && adoStateMetadataFresh(entry, c.stateMetadataNow()) {
		return entry.response(req), nil
	}
	return c.fetch(req, entry, hit, true, func(updated apiReadCacheEntry) {
		if req.Context().Err() != nil || !validADOStateMetadata(updated.Body) {
			return
		}
		updated.Stored = c.stateMetadataNow().Unix()
		updated.Snapshot = ""
		c.persist(map[string]apiReadCacheEntry{key: updated})
	})
}

func adoStateMetadataFresh(entry apiReadCacheEntry, now time.Time) bool {
	stored := entry.storedAt()
	return !now.Before(stored) && stored.Truncate(providers.ADOStateMetadataTTL).Equal(now.Truncate(providers.ADOStateMetadataTTL))
}

func validADOStateMetadata(body []byte) bool {
	var response struct {
		Value *[]struct{ Name, Category string }
	}
	if json.Unmarshal(body, &response) != nil || response.Value == nil {
		return false
	}
	for _, state := range *response.Value {
		if state.Name == "" || state.Category == "" {
			return false
		}
	}
	return true
}

func (c *apiReadCache) stateMetadataNow() time.Time {
	if c.metadataClock != nil {
		return c.metadataClock()
	}
	return time.Now()
}
