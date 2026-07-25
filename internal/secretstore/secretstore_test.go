package secretstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// countingStore is a fake Store recording every fetch so cache behavior is
// observable.
type countingStore struct {
	mu      sync.Mutex
	secrets map[string]string
	err     error
	fetches map[string]int
}

func newCountingStore(secrets map[string]string) *countingStore {
	return &countingStore{secrets: secrets, fetches: make(map[string]int)}
}

func (s *countingStore) FetchSecret(_ context.Context, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches[name]++
	if s.err != nil {
		return "", s.err
	}
	value, ok := s.secrets[name]
	if !ok {
		return "", errors.New("secret not found")
	}
	return value, nil
}

func (s *countingStore) fetchCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches[name]
}

func storeConfig(name string) instance.SecretStoreConfig {
	return instance.SecretStoreConfig{
		Name:     name,
		Kind:     instance.SecretStoreKindAzureKeyVault,
		VaultURI: "https://" + name + ".vault.azure.net",
		Auth:     &instance.SecretStoreAuthConfig{Kind: instance.SecretStoreAuthAzureCLI},
	}
}

func fakeBuiltRegistry(t *testing.T, fake Store, configs ...instance.SecretStoreConfig) *Registry {
	t.Helper()
	registry, err := newRegistry(configs, func(instance.SecretStoreConfig) (Store, error) {
		return fake, nil
	})
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	return registry
}

func TestRegistryResolvesDeclaredStore(t *testing.T) {
	fake := newCountingStore(map[string]string{"github-token": "kv-s3cr3t"})
	registry := fakeBuiltRegistry(t, fake, storeConfig("prod-kv"))

	got, err := registry.FetchSecret(context.Background(), "prod-kv/github-token")
	if err != nil {
		t.Fatalf("FetchSecret: %v", err)
	}
	if got != "kv-s3cr3t" {
		t.Fatalf("FetchSecret = %q, want %q", got, "kv-s3cr3t")
	}
}

func TestRegistryFailsClosedOnUnknownStore(t *testing.T) {
	fake := newCountingStore(nil)
	registry := fakeBuiltRegistry(t, fake, storeConfig("prod-kv"))

	_, err := registry.FetchSecret(context.Background(), "other-kv/github-token")
	if err == nil || !strings.Contains(err.Error(), "not declared under secretStores") {
		t.Fatalf("FetchSecret error = %v, want undeclared-store failure", err)
	}
	if n := fake.fetchCount("github-token"); n != 0 {
		t.Fatalf("undeclared store still fetched %d time(s)", n)
	}
}

func TestRegistryFailsClosedOnUnknownSecret(t *testing.T) {
	fake := newCountingStore(map[string]string{"github-token": "kv-s3cr3t"})
	registry := fakeBuiltRegistry(t, fake, storeConfig("prod-kv"))

	if _, err := registry.FetchSecret(context.Background(), "prod-kv/missing"); err == nil {
		t.Fatal("FetchSecret: want error for unknown secret, got nil")
	}
}

func TestRegistryFailsClosedOnMalformedRef(t *testing.T) {
	fake := newCountingStore(map[string]string{"github-token": "kv-s3cr3t"})
	registry := fakeBuiltRegistry(t, fake, storeConfig("prod-kv"))
	for _, ref := range []string{"", "prod-kv", "prod-kv/", "/github-token", "prod-kv/a/b"} {
		if _, err := registry.FetchSecret(context.Background(), ref); err == nil {
			t.Fatalf("FetchSecret(%q): want malformed-ref error, got nil", ref)
		}
	}
}

func TestNilRegistryFailsClosed(t *testing.T) {
	var registry *Registry
	if _, err := registry.FetchSecret(context.Background(), "prod-kv/github-token"); err == nil {
		t.Fatal("FetchSecret on nil registry: want error, got nil")
	}
}

func TestEmptyRegistryFailsEveryRefClosed(t *testing.T) {
	registry, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := registry.FetchSecret(context.Background(), "prod-kv/github-token"); err == nil {
		t.Fatal("FetchSecret on empty registry: want error, got nil")
	}
}

func TestNewRegistryRejectsDuplicateStores(t *testing.T) {
	_, err := newRegistry(
		[]instance.SecretStoreConfig{storeConfig("prod-kv"), storeConfig("prod-kv")},
		func(instance.SecretStoreConfig) (Store, error) { return newCountingStore(nil), nil },
	)
	if err == nil {
		t.Fatal("newRegistry: want duplicate-store error, got nil")
	}
}

func TestNewRegistrySurfacesStoreConstructionError(t *testing.T) {
	boom := errors.New("boom")
	_, err := newRegistry([]instance.SecretStoreConfig{storeConfig("prod-kv")},
		func(instance.SecretStoreConfig) (Store, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("newRegistry error = %v, want wrapped construction error", err)
	}
}

func TestRegistryCachesWithinTTL(t *testing.T) {
	fake := newCountingStore(map[string]string{"github-token": "kv-s3cr3t"})
	registry := fakeBuiltRegistry(t, fake, storeConfig("prod-kv"))

	for i := 0; i < 3; i++ {
		if _, err := registry.FetchSecret(context.Background(), "prod-kv/github-token"); err != nil {
			t.Fatalf("FetchSecret #%d: %v", i, err)
		}
	}
	if n := fake.fetchCount("github-token"); n != 1 {
		t.Fatalf("store fetched %d time(s) within TTL, want 1", n)
	}
}

func TestCachedStoreExpiresAfterTTL(t *testing.T) {
	fake := newCountingStore(map[string]string{"github-token": "kv-s3cr3t"})
	clock := time.Now()
	cache := newCachedStore(fake, 30*time.Second, func() time.Time { return clock })

	fetch := func() {
		t.Helper()
		if _, err := cache.FetchSecret(context.Background(), "github-token"); err != nil {
			t.Fatalf("FetchSecret: %v", err)
		}
	}
	fetch()
	clock = clock.Add(29 * time.Second)
	fetch()
	if n := fake.fetchCount("github-token"); n != 1 {
		t.Fatalf("store fetched %d time(s) before expiry, want 1", n)
	}
	clock = clock.Add(2 * time.Second)
	fetch()
	if n := fake.fetchCount("github-token"); n != 2 {
		t.Fatalf("store fetched %d time(s) after expiry, want 2", n)
	}
}

func TestCachedStoreCachesPerSecret(t *testing.T) {
	fake := newCountingStore(map[string]string{"a": "1", "b": "2"})
	cache := newCachedStore(fake, time.Minute, nil)
	for _, name := range []string{"a", "b", "a", "b"} {
		if _, err := cache.FetchSecret(context.Background(), name); err != nil {
			t.Fatalf("FetchSecret(%q): %v", name, err)
		}
	}
	if fake.fetchCount("a") != 1 || fake.fetchCount("b") != 1 {
		t.Fatalf("fetch counts a=%d b=%d, want 1 each", fake.fetchCount("a"), fake.fetchCount("b"))
	}
}

func TestCachedStoreNeverCachesErrors(t *testing.T) {
	fake := newCountingStore(map[string]string{"github-token": "kv-s3cr3t"})
	fake.err = errors.New("vault unavailable")
	cache := newCachedStore(fake, time.Minute, nil)

	if _, err := cache.FetchSecret(context.Background(), "github-token"); err == nil {
		t.Fatal("FetchSecret: want error, got nil")
	}
	fake.mu.Lock()
	fake.err = nil
	fake.mu.Unlock()
	if _, err := cache.FetchSecret(context.Background(), "github-token"); err != nil {
		t.Fatalf("FetchSecret after recovery: %v", err)
	}
	if n := fake.fetchCount("github-token"); n != 2 {
		t.Fatalf("store fetched %d time(s), want 2 (error not cached)", n)
	}
}

func TestCachedStoreHonorsCanceledContext(t *testing.T) {
	fake := newCountingStore(map[string]string{"github-token": "kv-s3cr3t"})
	cache := newCachedStore(fake, time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.FetchSecret(ctx, "github-token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchSecret error = %v, want context.Canceled", err)
	}
}

func TestCachedStoreConcurrentFetches(t *testing.T) {
	fake := newCountingStore(map[string]string{"github-token": "kv-s3cr3t"})
	cache := newCachedStore(fake, time.Minute, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := cache.FetchSecret(context.Background(), "github-token")
			if err != nil || value != "kv-s3cr3t" {
				t.Errorf("FetchSecret = %q, %v", value, err)
			}
		}()
	}
	wg.Wait()
	if n := fake.fetchCount("github-token"); n != 1 {
		t.Fatalf("store fetched %d time(s) under concurrency, want 1", n)
	}
}

func TestRegistryHonorsConfiguredCacheTTL(t *testing.T) {
	cfg := storeConfig("prod-kv")
	cfg.CacheTTLSeconds = 45
	registry, err := newRegistry([]instance.SecretStoreConfig{cfg, storeConfig("default-kv")},
		func(instance.SecretStoreConfig) (Store, error) { return newCountingStore(nil), nil })
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	configured, ok := registry.stores["prod-kv"].(*cachedStore)
	if !ok {
		t.Fatalf("store %T, want *cachedStore", registry.stores["prod-kv"])
	}
	if configured.ttl != 45*time.Second {
		t.Fatalf("configured ttl = %v, want 45s", configured.ttl)
	}
	defaulted := registry.stores["default-kv"].(*cachedStore)
	if defaulted.ttl != DefaultCacheTTL {
		t.Fatalf("default ttl = %v, want %v", defaulted.ttl, DefaultCacheTTL)
	}
}
