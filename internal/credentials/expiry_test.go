package credentials

import (
	"context"
	"errors"
	"testing"
	"time"
)

type expiryTestRegistrar struct{ registered [][]byte }

func (r *expiryTestRegistrar) Register(secret []byte) {
	r.registered = append(r.registered, append([]byte(nil), secret...))
}

// TestResolveWithExpiryThreadsSourceExpiry proves the DS10 plumbing end to
// end at the resolver: an expiring dynamic source's stated expiry travels
// with the value, a plain source and a static ref report none, and plain
// Resolve keeps returning just the value.
func TestResolveWithExpiryThreadsSourceExpiry(t *testing.T) {
	expires := time.Now().Add(50 * time.Minute).UTC()
	t.Setenv("EXPIRY_TEST_STATIC", "static-token-value")
	resolver, err := NewResolverWithExpiring(
		[]TokenRef{{Name: "static", Env: "EXPIRY_TEST_STATIC"}},
		nil,
		map[string]ResolveFunc{
			"plain": func(context.Context) (string, error) { return "plain-token-value", nil },
		},
		map[string]ExpiringResolveFunc{
			"minted": func(context.Context) (string, time.Time, error) { return "minted-token-value", expires, nil },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	expiring, ok := resolver.(ExpiringResolver)
	if !ok {
		t.Fatal("resolver does not implement ExpiringResolver")
	}
	ctx := context.Background()

	value, expiresAt, err := expiring.ResolveWithExpiry(ctx, "minted")
	if err != nil || value != "minted-token-value" || !expiresAt.Equal(expires) {
		t.Fatalf("minted: value=%q expiresAt=%v err=%v", value, expiresAt, err)
	}
	for _, name := range []string{"plain", "static"} {
		value, expiresAt, err := expiring.ResolveWithExpiry(ctx, name)
		if err != nil || value == "" || !expiresAt.IsZero() {
			t.Fatalf("%s: value=%q expiresAt=%v err=%v (want zero expiry)", name, value, expiresAt, err)
		}
	}
	if value, err := resolver.Resolve(ctx, "minted"); err != nil || value != "minted-token-value" {
		t.Fatalf("plain Resolve of expiring source: value=%q err=%v", value, err)
	}
}

func TestNewResolverWithExpiringRejectsCollisionsAndNilSources(t *testing.T) {
	fn := func(context.Context) (string, time.Time, error) { return "v", time.Time{}, nil }
	if _, err := NewResolverWithExpiring(nil, nil,
		map[string]ResolveFunc{"dup": func(context.Context) (string, error) { return "v", nil }},
		map[string]ExpiringResolveFunc{"dup": fn},
	); err == nil {
		t.Fatal("duplicate name across source maps must be rejected")
	}
	if _, err := NewResolverWithExpiring(nil, nil, nil,
		map[string]ExpiringResolveFunc{"nil": nil}); err == nil {
		t.Fatal("nil expiring source must be rejected")
	}
	if _, err := NewResolverWithExpiring([]TokenRef{{Name: "dup", Env: "X"}}, nil, nil,
		map[string]ExpiringResolveFunc{"dup": fn}); err == nil {
		t.Fatal("duplicate name across refs and expiring sources must be rejected")
	}
}

// TestMaterializeCarriesExpiryIntoTheSet proves the injector threads a
// per-capability expiry into the Set (Set.Expiry) when the resolver states
// one, registers the value with the registrar BEFORE returning, and reports
// no expiry for capabilities whose source states none.
func TestMaterializeCarriesExpiryIntoTheSet(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC()
	resolver, err := NewResolverWithExpiring(nil, nil,
		map[string]ResolveFunc{
			"static-ref": func(context.Context) (string, error) { return "static-secret-value", nil },
		},
		map[string]ExpiringResolveFunc{
			"minted-ref": func(context.Context) (string, time.Time, error) { return "minted-secret-value", expires, nil },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	registrar := &expiryTestRegistrar{}
	injector, err := NewInjector(resolver, []Grant{
		{Capability: "repo:push", Ref: "minted-ref"},
		{Capability: "agent:model", Ref: "static-ref"},
	}, registrar)
	if err != nil {
		t.Fatal(err)
	}
	set, err := injector.Materialize(context.Background(), []string{"repo:push", "agent:model"})
	if err != nil {
		t.Fatal(err)
	}

	if expiresAt, ok := set.Expiry("repo:push"); !ok || !expiresAt.Equal(expires) {
		t.Fatalf("repo:push expiry = %v, %v; want %v, true", expiresAt, ok, expires)
	}
	if _, ok := set.Expiry("agent:model"); ok {
		t.Fatal("agent:model reports an expiry its source never stated")
	}
	if _, ok := set.Expiry("undeclared"); ok {
		t.Fatal("an unmaterialized capability reports an expiry")
	}
	if len(registrar.registered) != 2 {
		t.Fatalf("registrar saw %d values, want 2 (every value registers before Materialize returns)", len(registrar.registered))
	}
	if _, err := set.Token(context.Background(), "undeclared"); !errors.Is(err, ErrUndeclaredCapability) {
		t.Fatalf("undeclared capability error = %v, want ErrUndeclaredCapability", err)
	}
}

func TestMaterializedExpiringCredentialRefreshReResolves(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC()
	calls := 0
	resolver, err := NewResolverWithExpiring(nil, nil, nil,
		map[string]ExpiringResolveFunc{
			"minted-ref": func(context.Context) (string, time.Time, error) {
				calls++
				if calls == 1 {
					return "delivered-token-value", expires, nil
				}
				return "retry-token-value", expires.Add(time.Hour), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	registrar := &expiryTestRegistrar{}
	injector, err := NewInjector(resolver, []Grant{{Capability: "agent:model", Ref: "minted-ref"}}, registrar)
	if err != nil {
		t.Fatal(err)
	}
	set, err := injector.Materialize(context.Background(), []string{"agent:model"})
	if err != nil {
		t.Fatal(err)
	}

	if got, err := set.Token(context.Background(), "agent:model"); err != nil || got != "delivered-token-value" {
		t.Fatalf("Token = %q, %v; want delivered value before refresh", got, err)
	}
	if got, err := set.Refresh(context.Background(), "agent:model"); err != nil || got != "retry-token-value" {
		t.Fatalf("Refresh = %q, %v; want re-resolved retry value", got, err)
	}
	if calls != 2 {
		t.Fatalf("source calls = %d, want initial materialize plus forced refresh", calls)
	}
	if expiresAt, ok := set.Expiry("agent:model"); !ok || !expiresAt.Equal(expires.Add(time.Hour)) {
		t.Fatalf("refreshed expiry = %v, %v; want %v, true", expiresAt, ok, expires.Add(time.Hour))
	}
	if len(registrar.registered) != 2 {
		t.Fatalf("registrar saw %d values, want delivered and refreshed tokens", len(registrar.registered))
	}
}
