package apireadcache

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

// Cache only bounded complete bodies. Large responses remain streaming to the
// provider and retain ownership of the original closer; they never enter memory
// or disk cache custody in full.
func cacheReadResponse(resp *http.Response, snapshot bool, save func(apiReadCacheEntry)) (*http.Response, error) {
	etag, modified := resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	if (etag == "" && modified == "" && !snapshot) || strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-store") || resp.Header.Get("Vary") == "*" {
		return resp, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, apiReadCacheMaxBytes+1))
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if len(body) > apiReadCacheMaxBytes {
		resp.Body = &prefixedReadBody{Reader: io.MultiReader(bytes.NewReader(body), resp.Body), closer: resp.Body}
		return resp, nil
	}
	_ = resp.Body.Close()
	save(apiReadCacheEntry{ETag: etag, LastModified: modified, Continuation: resp.Header.Get("X-MS-ContinuationToken"), Link: resp.Header.Get("Link"), Type: resp.Header.Get("Content-Type"), Body: body, Stored: time.Now().Unix()})
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

type prefixedReadBody struct {
	io.Reader
	closer io.Closer
}

func (b *prefixedReadBody) Close() error { return b.closer.Close() }

// ADO validators reuse bytes but still perform a provider request. Preserve its
// reported cost/delay rather than treating a 304 as a free snapshot hit.
func (c *apiReadCache) validatedResponse(req *http.Request, entry apiReadCacheEntry, validation *http.Response) *http.Response {
	response := entry.response(req)
	if c.provider == providers.ProviderADO {
		response.Header.Del(providers.QuotaCacheHitHeader)
		for _, name := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-Delay", "Retry-After"} {
			if value := validation.Header.Get(name); value != "" {
				response.Header.Set(name, value)
			}
		}
	}
	return response
}
