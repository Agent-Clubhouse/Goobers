// Package secretstore resolves store-backed token refs
// ("<storeName>/<secretName>") against the external secret stores an instance
// declares under secretStores (#683, SEC-010). The Registry is the production
// credentials.StoreResolver: it is built once by a composition root from the
// validated instance config and threaded to every token-ref consumer, so a
// store: ref resolves wherever an env/file ref does. Resolution is fail-closed
// end to end — an undeclared store or an unresolvable secret is an error,
// never a fallback to an unauthenticated or unconfigured path.
package secretstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// Store fetches one named secret from an external secret store. Implementations
// must honor context cancellation and must never log or persist secret values.
type Store interface {
	FetchSecret(ctx context.Context, name string) (string, error)
}

// DefaultCacheTTL bounds how long a fetched secret is served from memory when
// a store declares no cacheTTLSeconds: long enough that a burst of stage
// starts costs one vault round-trip, short enough that rotation in the store
// is picked up without a daemon restart.
const DefaultCacheTTL = 5 * time.Minute

// fetchTimeout bounds a single vault round-trip (mirroring internal/githubapp's
// mintTimeout). The cache holds its mutex across the fetch, so an unbounded
// call to an unreachable store would serialize every other ref behind it and
// could wedge a daemon-start resolution path whose caller passed no deadline;
// this cap guarantees the lock is always released.
const fetchTimeout = 30 * time.Second

// Registry resolves "<storeName>/<secretName>" refs across the declared
// stores. It satisfies credentials.StoreResolver structurally, keeping the
// credentials package free of any vendor dependency.
type Registry struct {
	stores map[string]Store
}

// NewRegistry builds the registry from the validated secretStores config,
// wrapping each store in a TTL cache. An empty config yields a working
// registry that fails every ref closed ("not declared"), so callers always
// thread a non-nil *Registry and unconfigured instances behave exactly as
// before — no ref can name a store that was never declared.
func NewRegistry(configs []instance.SecretStoreConfig) (*Registry, error) {
	return newRegistry(configs, newAzureKeyVaultStore)
}

// newRegistry is NewRegistry with the vendor store constructor injected so
// contract tests exercise the registry and cache against a fake Store.
func newRegistry(configs []instance.SecretStoreConfig, build func(instance.SecretStoreConfig) (Store, error)) (*Registry, error) {
	stores := make(map[string]Store, len(configs))
	for _, cfg := range configs {
		// instance.Config.Validate already rejects duplicates and unknown
		// kinds; re-check here so a registry built from an unvalidated slice
		// still fails closed instead of silently shadowing a store.
		if _, dup := stores[cfg.Name]; dup {
			return nil, fmt.Errorf("secretstore: store %q is declared more than once", cfg.Name)
		}
		store, err := build(cfg)
		if err != nil {
			return nil, fmt.Errorf("secretstore: store %q: %w", cfg.Name, err)
		}
		ttl := DefaultCacheTTL
		if cfg.CacheTTLSeconds > 0 {
			ttl = time.Duration(cfg.CacheTTLSeconds) * time.Second
		}
		stores[cfg.Name] = newCachedStore(store, ttl, time.Now)
	}
	return &Registry{stores: stores}, nil
}

// FetchSecret resolves a full "<storeName>/<secretName>" ref. It re-parses
// the ref rather than trusting upstream validation, so a malformed ref that
// slipped past config load still fails closed here.
func (r *Registry) FetchSecret(ctx context.Context, ref string) (string, error) {
	if r == nil {
		return "", errors.New("secretstore: no secret stores configured")
	}
	storeName, secretName, ok := strings.Cut(ref, "/")
	if !ok || storeName == "" || secretName == "" || strings.Contains(secretName, "/") {
		return "", fmt.Errorf("secretstore: store ref %q must have the form \"<storeName>/<secretName>\"", ref)
	}
	store, ok := r.stores[storeName]
	if !ok {
		return "", fmt.Errorf("secretstore: store ref %q names secret store %q, which is not declared under secretStores", ref, storeName)
	}
	value, err := store.FetchSecret(ctx, secretName)
	if err != nil {
		return "", fmt.Errorf("secretstore: store %q: secret %q: %w", storeName, secretName, err)
	}
	return value, nil
}

// cachedStore serves repeat fetches of the same secret from memory for up to
// ttl. Errors are never cached, and the mutex is held across the fetch —
// matching providers' cachedADOBearerSource — so concurrent first fetches of
// one secret cost a single vault round-trip instead of a stampede.
type cachedStore struct {
	inner Store
	ttl   time.Duration
	now   func() time.Time

	mu     sync.Mutex
	values map[string]cachedSecret
}

type cachedSecret struct {
	value     string
	fetchedAt time.Time
}

func newCachedStore(inner Store, ttl time.Duration, now func() time.Time) *cachedStore {
	if now == nil {
		now = time.Now
	}
	return &cachedStore{inner: inner, ttl: ttl, now: now, values: make(map[string]cachedSecret)}
}

func (c *cachedStore) FetchSecret(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.values[name]; ok && c.now().Sub(entry.fetchedAt) < c.ttl {
		return entry.value, nil
	}
	// Bound the round-trip so a hung/unreachable store cannot hold the mutex
	// (and thus block every other ref) indefinitely, even when the caller's
	// context carries no deadline.
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	value, err := c.inner.FetchSecret(fetchCtx, name)
	if err != nil {
		return "", err
	}
	c.values[name] = cachedSecret{value: value, fetchedAt: c.now()}
	return value, nil
}
