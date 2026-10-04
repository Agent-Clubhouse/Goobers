package apireadcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/goobers/goobers/providers"
)

// Scope names an already authorized gaggle source and policy generation. Cache
// custody is never permission: callers must authorize every read before using it.
// Automation and interactive bindings must have different names even if their
// credentials happen to be equal. Empty or malformed scope disables caching.
type Scope struct {
	Gaggle     string
	Binding    string
	Generation string
}

func (s Scope) key() string {
	for _, value := range []string{s.Gaggle, s.Binding, s.Generation} {
		if value == "" || len(value) > 512 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return ""
		}
	}
	raw, _ := json.Marshal(s)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ScopedClient shares bounded GET snapshots only within one gaggle/source/policy
// partition. Provider endpoint, credentials and all representation headers further
// partition each entry. Missing scope is a pass-through, never a global cache.
func ScopedClient(directory, snapshot string, scope Scope, provider providers.ProviderKind, inner providers.HTTPClient) providers.HTTPClient {
	if inner == nil {
		inner = &http.Client{Timeout: apiReadHTTPTimeout}
	}
	partition := scope.key()
	if partition == "" || (provider != providers.ProviderGitHub && provider != providers.ProviderADO) {
		directory = ""
	}
	cache := newAPIReadCache(directory, scopedSnapshot(partition, snapshot), inner)
	cache.partition = partition
	cache.provider = provider
	return cache
}

// ScopedGitHubOption installs the same cache around the provider's existing
// transport, preserving custom transports and retry/redirect behavior.
func ScopedGitHubOption(directory, snapshot string, scope Scope) func(*providers.GitHubProvider) {
	return func(p *providers.GitHubProvider) {
		p.Client = ScopedClient(directory, snapshot, scope, providers.ProviderGitHub, p.Client)
	}
}

func scopedSnapshot(partition, snapshot string) string {
	if snapshot == "" {
		return ""
	}
	return "gaggle-v1:" + partition + ":" + snapshot
}

// InvalidateScopedSnapshot invalidates only this partition's evaluation reads.
func InvalidateScopedSnapshot(directory, snapshot string, scope Scope) error {
	if scope.key() == "" {
		return nil
	}
	return InvalidateSnapshot(directory, scopedSnapshot(scope.key(), snapshot))
}

func scopedRequestKey(partition string, req *http.Request) string {
	// Headers can choose different provider representations. Hash canonical JSON,
	// including the full credential fingerprint rather than storing raw secrets.
	headers := req.Header.Clone()
	headers.Del("If-None-Match")
	headers.Del("If-Modified-Since")
	raw, _ := json.Marshal(struct {
		Partition, URL string
		Headers        http.Header
	}{partition, req.URL.String(), headers})
	sum := sha256.Sum256(raw)
	return "gaggle-v1:" + hex.EncodeToString(sum[:])
}

func (c *apiReadCache) listRequest(req *http.Request) bool {
	if c.provider != providers.ProviderADO {
		return isProviderListRequest(req)
	}
	path := strings.ToLower(strings.TrimSuffix(req.URL.Path, "/"))
	return strings.Contains(path, "/_apis/git/repositories/") && strings.HasSuffix(path, "/pullrequests")
}

func scopedCacheable(req *http.Request) bool {
	if req.URL == nil || req.URL.User != nil || (req.Body != nil && req.Body != http.NoBody) {
		return false
	}
	for _, name := range []string{"Range", "If-None-Match", "If-Modified-Since", "If-Match", "Cache-Control", "Pragma"} {
		if req.Header.Get(name) != "" {
			return false
		}
	}
	return true
}
