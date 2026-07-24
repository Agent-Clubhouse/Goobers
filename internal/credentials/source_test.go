package credentials

import (
	"context"
	"errors"
	"os"
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
		{"neither env nor file", []TokenRef{{Name: "gh"}}},
		{"both env and file", []TokenRef{{Name: "gh", Env: "X", File: "y"}}},
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func TestResolverWithSourcesRoutesDynamicNames(t *testing.T) {
	t.Setenv("GH_TOKEN_STATIC_TEST", "static-token")
	minted := 0
	r, err := NewResolverWithSources(
		[]TokenRef{{Name: "static", Env: "GH_TOKEN_STATIC_TEST"}},
		map[string]ResolveFunc{"acme/web": func(context.Context) (string, error) {
			minted++
			return "minted-token", nil
		}},
	)
	if err != nil {
		t.Fatalf("NewResolverWithSources: %v", err)
	}
	got, err := r.Resolve(context.Background(), "acme/web")
	if err != nil {
		t.Fatalf("Resolve dynamic: %v", err)
	}
	if got != "minted-token" || minted != 1 {
		t.Fatalf("Resolve = %q (minted %d), want minted-token via the source", got, minted)
	}
	// A second resolve consults the source again — its own cache decides.
	if _, err := r.Resolve(context.Background(), "acme/web"); err != nil || minted != 2 {
		t.Fatalf("second Resolve minted %d (err %v), want per-resolve consultation", minted, err)
	}
	if got, err := r.Resolve(context.Background(), "static"); err != nil || got != "static-token" {
		t.Fatalf("static ref = %q, %v — dynamic sources must not shadow refs", got, err)
	}
}

func TestResolverWithSourcesFailsClosed(t *testing.T) {
	r, err := NewResolverWithSources(nil, map[string]ResolveFunc{
		"minty": func(context.Context) (string, error) {
			return "", errors.New("mint refused")
		},
		"empty": func(context.Context) (string, error) {
			return "   ", nil
		},
	})
	if err != nil {
		t.Fatalf("NewResolverWithSources: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "minty"); err == nil {
		t.Fatal("Resolve: want mint error surfaced, got nil")
	}
	if _, err := r.Resolve(context.Background(), "empty"); !errors.Is(err, ErrTokenRefEmpty) {
		t.Fatalf("Resolve empty mint = %v, want ErrTokenRefEmpty", err)
	}
	if _, err := r.Resolve(context.Background(), "unknown"); !errors.Is(err, ErrTokenRefNotFound) {
		t.Fatalf("Resolve unknown = %v, want ErrTokenRefNotFound", err)
	}
}

func TestNewResolverWithSourcesRejectsMalformedSources(t *testing.T) {
	noop := func(context.Context) (string, error) { return "x", nil }
	cases := []struct {
		name    string
		refs    []TokenRef
		sources map[string]ResolveFunc
	}{
		{"nil func", nil, map[string]ResolveFunc{"a": nil}},
		{"empty name", nil, map[string]ResolveFunc{"": noop}},
		{"collides with ref", []TokenRef{{Name: "a", Env: "X"}}, map[string]ResolveFunc{"a": noop}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewResolverWithSources(tc.refs, tc.sources); err == nil {
				t.Fatal("NewResolverWithSources: want error, got nil")
			}
		})
	}
}
