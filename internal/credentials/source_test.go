package credentials

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestResolverResolvesFromEnv(t *testing.T) {
	t.Setenv("GH_TOKEN_TEST", "  s3cr3t-value  ")
	r, err := NewResolver([]TokenRef{{Name: "gh", Env: "GH_TOKEN_TEST"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	got, err := r.Resolve(context.Background(), "gh")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "s3cr3t-value" {
		t.Fatalf("Resolve = %q, want trimmed %q", got, "s3cr3t-value")
	}
}

func TestResolverMissingEnvFailsClosed(t *testing.T) {
	r, err := NewResolver([]TokenRef{{Name: "gh", Env: "GH_TOKEN_DEFINITELY_UNSET_XYZ"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "gh"); err == nil {
		t.Fatal("Resolve: want error for unset env var, got nil")
	}
}

func TestResolverEmptyValueFailsClosed(t *testing.T) {
	t.Setenv("GH_TOKEN_EMPTY_TEST", "   ")
	r, err := NewResolver([]TokenRef{{Name: "gh", Env: "GH_TOKEN_EMPTY_TEST"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	_, err = r.Resolve(context.Background(), "gh")
	if !errors.Is(err, ErrTokenRefEmpty) {
		t.Fatalf("Resolve error = %v, want ErrTokenRefEmpty", err)
	}
}

func TestResolverUnknownRefFailsClosed(t *testing.T) {
	r, err := NewResolver(nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	_, err = r.Resolve(context.Background(), "missing")
	if !errors.Is(err, ErrTokenRefNotFound) {
		t.Fatalf("Resolve error = %v, want ErrTokenRefNotFound", err)
	}
}

func TestResolverHonorsCanceledContext(t *testing.T) {
	t.Setenv("GH_TOKEN_CANCELED_TEST", "secret")
	r, err := NewResolver([]TokenRef{{Name: "gh", Env: "GH_TOKEN_CANCELED_TEST"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = r.Resolve(ctx, "gh")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve error = %v, want context.Canceled", err)
	}
}

func TestNewResolverRejectsMalformedRefs(t *testing.T) {
	cases := []struct {
		name string
		refs []TokenRef
	}{
		{"no name", []TokenRef{{Env: "X"}}},
		{"no source", []TokenRef{{Name: "gh"}}},
		{"both env and file", []TokenRef{{Name: "gh", Env: "X", File: "y"}}},
		{"both env and store", []TokenRef{{Name: "gh", Env: "X", Store: "kv/gh"}}},
		{"both file and store", []TokenRef{{Name: "gh", File: "y", Store: "kv/gh"}}},
		{"duplicate name", []TokenRef{{Name: "gh", Env: "X"}, {Name: "gh", Env: "Y"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewResolver(tc.refs); err == nil {
				t.Fatal("NewResolver: want error, got nil")
			}
		})
	}
}

// fakeStoreResolver is a StoreResolver test double recording the refs it was
// asked for.
type fakeStoreResolver struct {
	secrets map[string]string
	err     error
	asked   []string
}

func (f *fakeStoreResolver) FetchSecret(_ context.Context, ref string) (string, error) {
	f.asked = append(f.asked, ref)
	if f.err != nil {
		return "", f.err
	}
	value, ok := f.secrets[ref]
	if !ok {
		return "", errors.New("secretstore: not declared")
	}
	return value, nil
}

func TestResolverResolvesStoreRef(t *testing.T) {
	stores := &fakeStoreResolver{secrets: map[string]string{"prod-kv/github-token": "  kv-s3cr3t\n"}}
	r, err := NewResolverWithStores([]TokenRef{{Name: "gh", Store: "prod-kv/github-token"}}, stores)
	if err != nil {
		t.Fatalf("NewResolverWithStores: %v", err)
	}
	got, err := r.Resolve(context.Background(), "gh")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "kv-s3cr3t" {
		t.Fatalf("Resolve = %q, want trimmed %q", got, "kv-s3cr3t")
	}
	if len(stores.asked) != 1 || stores.asked[0] != "prod-kv/github-token" {
		t.Fatalf("store asked for %v, want the full ref exactly once", stores.asked)
	}
}

func TestNewResolverFailsClosedOnStoreRefWithoutStores(t *testing.T) {
	_, err := NewResolver([]TokenRef{{Name: "gh", Store: "prod-kv/github-token"}})
	if err == nil {
		t.Fatal("NewResolver: want error for store ref without a store resolver, got nil")
	}
	if !strings.Contains(err.Error(), "no secret store resolver is configured") {
		t.Fatalf("NewResolver error = %v, want it to name the missing store resolver", err)
	}
}

func TestResolverStoreErrorFailsClosed(t *testing.T) {
	stores := &fakeStoreResolver{err: errors.New("boom")}
	r, err := NewResolverWithStores([]TokenRef{{Name: "gh", Store: "prod-kv/github-token"}}, stores)
	if err != nil {
		t.Fatalf("NewResolverWithStores: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "gh"); err == nil {
		t.Fatal("Resolve: want store fetch error, got nil")
	}
}

func TestResolverStoreEmptyValueFailsClosed(t *testing.T) {
	stores := &fakeStoreResolver{secrets: map[string]string{"prod-kv/blank": "   "}}
	r, err := NewResolverWithStores([]TokenRef{{Name: "gh", Store: "prod-kv/blank"}}, stores)
	if err != nil {
		t.Fatalf("NewResolverWithStores: %v", err)
	}
	_, err = r.Resolve(context.Background(), "gh")
	if !errors.Is(err, ErrTokenRefEmpty) {
		t.Fatalf("Resolve error = %v, want ErrTokenRefEmpty", err)
	}
}

func TestResolverWithStoresStillResolvesEnvRefs(t *testing.T) {
	t.Setenv("GH_TOKEN_MIXED_TEST", "env-value")
	stores := &fakeStoreResolver{secrets: map[string]string{"prod-kv/github-token": "kv-value"}}
	r, err := NewResolverWithStores([]TokenRef{
		{Name: "gh-env", Env: "GH_TOKEN_MIXED_TEST"},
		{Name: "gh-store", Store: "prod-kv/github-token"},
	}, stores)
	if err != nil {
		t.Fatalf("NewResolverWithStores: %v", err)
	}
	if got, err := r.Resolve(context.Background(), "gh-env"); err != nil || got != "env-value" {
		t.Fatalf("Resolve(gh-env) = %q, %v; want env-value", got, err)
	}
	if got, err := r.Resolve(context.Background(), "gh-store"); err != nil || got != "kv-value" {
		t.Fatalf("Resolve(gh-store) = %q, %v; want kv-value", got, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
