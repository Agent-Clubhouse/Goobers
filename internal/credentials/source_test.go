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
