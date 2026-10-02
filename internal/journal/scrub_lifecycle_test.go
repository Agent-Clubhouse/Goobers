package journal

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRegistryClock is a concurrency-safe settable clock for lifecycle tests.
type fakeRegistryClock struct{ unixNano atomic.Int64 }

func newFakeRegistryClock(t time.Time) *fakeRegistryClock {
	c := &fakeRegistryClock{}
	c.unixNano.Store(t.UnixNano())
	return c
}

func (c *fakeRegistryClock) Now() time.Time          { return time.Unix(0, c.unixNano.Load()) }
func (c *fakeRegistryClock) Advance(d time.Duration) { c.unixNano.Add(int64(d)) }
func (c *fakeRegistryClock) Set(t time.Time)         { c.unixNano.Store(t.UnixNano()) }
func newClockedRegistry(c *fakeRegistryClock) *RegistryScrubber {
	reg := NewRegistryScrubber()
	reg.now = c.Now
	return reg
}

func registrySize(reg *RegistryScrubber) int {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return len(reg.secrets)
}

func redacts(reg *RegistryScrubber, secret string) bool {
	return !bytes.Contains(reg.Scrub([]byte("x "+secret+" y")), []byte(secret))
}

// TestRegistryScrubberRetiresOnlyAfterGrace pins the #2656 lifecycle: an
// expiring value stays redacted through its expiry AND the whole grace window,
// and is retired only once both have passed.
func TestRegistryScrubberRetiresOnlyAfterGrace(t *testing.T) {
	start := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	clock := newFakeRegistryClock(start)
	reg := newClockedRegistry(clock)
	const token = "ghs_expiring-token-000001"
	reg.RegisterUntil([]byte(token), start.Add(time.Hour))

	for _, at := range []time.Duration{0, time.Hour, time.Hour + RegistryRetirementGrace - time.Nanosecond} {
		clock.Set(start.Add(at))
		if !redacts(reg, token) {
			t.Fatalf("value not redacted at +%s, inside expiry+grace", at)
		}
	}
	clock.Set(start.Add(time.Hour + RegistryRetirementGrace))
	if redacts(reg, token) {
		t.Fatalf("value still redacted after expiry+grace; registry never retires")
	}
	if n := registrySize(reg); n != 0 {
		t.Fatalf("retired entry still held: size=%d", n)
	}
}

// TestRegistryScrubberExpiryIsACredentialFact pins the re-registration rules:
// the latest stated expiry wins, an earlier one never shortens it, a plain
// Register (a caller that re-resolved the value without its expiry) neither
// pins nor shortens a known expiry, and a value no caller stated an expiry for
// is permanent.
func TestRegistryScrubberExpiryIsACredentialFact(t *testing.T) {
	start := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	clock := newFakeRegistryClock(start)
	reg := newClockedRegistry(clock)

	const static = "static-credential-no-expiry"
	reg.Register([]byte(static))

	const reresolved = "minted-then-reresolved-token"
	reg.RegisterUntil([]byte(reresolved), start.Add(time.Hour))
	reg.Register([]byte(reresolved)) // a consumer that re-resolved it

	const resolvedFirst = "resolved-before-mint-recorded"
	reg.Register([]byte(resolvedFirst))
	reg.RegisterUntil([]byte(resolvedFirst), start.Add(time.Hour))

	const extended = "extended-rotating-credential"
	reg.RegisterUntil([]byte(extended), start.Add(time.Hour))
	reg.RegisterUntil([]byte(extended), start.Add(3*time.Hour))
	reg.RegisterUntil([]byte(extended), start.Add(time.Minute))

	clock.Set(start.Add(time.Hour + RegistryRetirementGrace - time.Nanosecond))
	for _, v := range []string{static, reresolved, resolvedFirst, extended} {
		if !redacts(reg, v) {
			t.Fatalf("%q retired inside its expiry+grace window", v)
		}
	}
	clock.Set(start.Add(time.Hour + RegistryRetirementGrace))
	if redacts(reg, reresolved) || redacts(reg, resolvedFirst) {
		t.Fatalf("a plain re-registration pinned a value whose expiry was known")
	}
	if !redacts(reg, extended) {
		t.Fatalf("a shorter re-registration shortened an extended lifetime")
	}
	clock.Set(start.Add(3*time.Hour + RegistryRetirementGrace))
	if redacts(reg, extended) {
		t.Fatalf("extended value outlived its extended deadline")
	}
	if !redacts(reg, static) {
		t.Fatalf("a value with no stated expiry was retired")
	}
}

// TestRegistryScrubberTargetCacheTracksRegistryChanges: the cached sorted
// target list is reused between changes and invalidated by a registration, so a
// value registered after a scrub is redacted by the very next scrub.
func TestRegistryScrubberTargetCacheTracksRegistryChanges(t *testing.T) {
	reg := NewRegistryScrubber()
	reg.Register([]byte("first-secret-value"))
	_ = reg.Scrub([]byte("warm"))
	first := reg.redactionTargets()
	if second := reg.redactionTargets(); &first[0] != &second[0] {
		t.Fatalf("target list rebuilt with no registry change")
	}
	reg.Register([]byte("second-secret-value"))
	if !redacts(reg, "second-secret-value") || !redacts(reg, "first-secret-value") {
		t.Fatalf("cache not invalidated by a new registration")
	}
	// Re-registering an existing permanent value is not a change.
	cached := reg.redactionTargets()
	reg.Register([]byte("second-secret-value"))
	if again := reg.redactionTargets(); &cached[0] != &again[0] {
		t.Fatalf("idempotent re-registration invalidated the cache")
	}
}

// TestRegistryScrubberRegisterSecretUntilFallsBackToPermanent: a registrar
// without expiry support receives a plain (permanent) Register.
func TestRegistryScrubberRegisterSecretUntilFallsBackToPermanent(t *testing.T) {
	var plain plainRegistrar
	RegisterSecretUntil(&plain, []byte("value-123456"), time.Now())
	if len(plain.got) != 1 {
		t.Fatalf("plain registrar got %d registrations, want 1", len(plain.got))
	}
	reg := NewRegistryScrubber()
	RegisterSecretUntil(reg, []byte("value-123456"), time.Now().Add(time.Hour))
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for _, e := range reg.secrets {
		if e.expiresAt.IsZero() {
			t.Fatalf("expiry dropped for an expiring registrar")
		}
	}
}

type plainRegistrar struct{ got [][]byte }

func (p *plainRegistrar) Register(b []byte) { p.got = append(p.got, b) }

// TestRegistryScrubberRotationSoak simulates a daemon lifetime of overlapping
// hourly token rotations (each new token minted ten minutes before the previous
// one expires) under concurrent scrubs. It asserts the two #2656 properties:
// every token inside its expiry+grace window is always redacted, and the
// registry stays within the documented bound instead of growing for the
// daemon's lifetime.
func TestRegistryScrubberRotationSoak(t *testing.T) {
	const (
		ttl       = time.Hour
		interval  = 50 * time.Minute // overlap: mint ten minutes before expiry
		rotations = 200              // ~1 week of rotations
		scrubbers = 4
	)
	start := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	clock := newFakeRegistryClock(start)
	reg := newClockedRegistry(clock)
	const static = "static-webhook-secret-value"
	reg.Register([]byte(static))

	type minted struct {
		value     string
		expiresAt time.Time
	}
	var (
		mu     sync.Mutex
		tokens []minted
	)
	maxLive := int((ttl+RegistryRetirementGrace)/interval) + 2 // + boundary + static

	var wg sync.WaitGroup
	stop := make(chan struct{})
	failures := make(chan string, scrubbers)
	for w := 0; w < scrubbers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				mu.Lock()
				snapshot := append([]minted(nil), tokens...)
				mu.Unlock()
				for _, tok := range snapshot {
					// Read the clock AFTER the scrub: if the token is
					// still in its window then, it was in its window for
					// the whole scrub (the clock only moves forward).
					ok := redacts(reg, tok.value)
					now := clock.Now()
					if !ok && now.Before(tok.expiresAt.Add(RegistryRetirementGrace)) {
						failures <- fmt.Sprintf("token %s leaked inside its window at %s", tok.value, now)
						return
					}
				}
				if !redacts(reg, static) {
					failures <- "permanent secret leaked"
					return
				}
			}
		}()
	}

	for i := 0; i < rotations; i++ {
		now := clock.Now()
		tok := minted{value: fmt.Sprintf("ghs_rotation-token-%06d", i), expiresAt: now.Add(ttl)}
		reg.RegisterUntil([]byte(tok.value), tok.expiresAt)
		// Consumers that re-resolve the current token register it again
		// without its expiry; that must not pin it past retirement.
		reg.Register([]byte(tok.value))
		mu.Lock()
		tokens = append(tokens, tok)
		// Keep only tokens a scrubber could still need to check.
		for len(tokens) > maxLive*2 {
			tokens = tokens[1:]
		}
		mu.Unlock()
		if n := registrySize(reg); n > maxLive {
			close(stop)
			wg.Wait()
			t.Fatalf("rotation %d: registry holds %d entries, documented bound %d", i, n, maxLive)
		}
		// Let the scrubbers observe this registry generation before the
		// clock moves on, so concurrent scrubs span many rotations.
		for k := 0; k < scrubbers; k++ {
			runtime.Gosched()
			_ = reg.Scrub([]byte(tok.value))
		}
		clock.Advance(interval)
	}
	close(stop)
	wg.Wait()
	select {
	case f := <-failures:
		t.Fatal(f)
	default:
	}
}

// BenchmarkRegistryScrubberSteadyState measures a scrub against a registry
// that has stabilised under rotation: with the cached target list it does no
// sort or allocation for the targets, and the target count is bounded.
func BenchmarkRegistryScrubberSteadyState(b *testing.B) {
	start := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	clock := newFakeRegistryClock(start)
	reg := newClockedRegistry(clock)
	for i := 0; i < 1000; i++ { // 1000 hourly rotations, ~6 weeks
		reg.RegisterUntil([]byte(fmt.Sprintf("ghs_rotation-token-%06d", i)), clock.Now().Add(time.Hour))
		clock.Advance(time.Hour)
	}
	payload := bytes.Repeat([]byte(`{"type":"stage.completed","detail":"ok"} `), 64)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = reg.Scrub(payload)
		}
	})
	b.ReportMetric(float64(registrySize(reg)), "entries")
}
