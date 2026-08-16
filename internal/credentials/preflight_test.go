package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preflightResolver(t *testing.T, refs []TokenRef) Preflighter {
	t.Helper()
	resolver, err := NewResolver(refs)
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	p, ok := resolver.(Preflighter)
	if !ok {
		t.Fatalf("resolver does not implement Preflighter")
	}
	return p
}

func TestPreflightPassesWhenEveryRefResolves(t *testing.T) {
	t.Setenv("GOOBERS_TEST_PREFLIGHT_OK", "value")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-value\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	p := preflightResolver(t, []TokenRef{
		{Name: "env-ref", Env: "GOOBERS_TEST_PREFLIGHT_OK"},
		{Name: "file-ref", File: tokenFile},
	})
	if err := p.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() = %v, want nil", err)
	}
}

func TestPreflightNoRefsPasses(t *testing.T) {
	p := preflightResolver(t, nil)
	if err := p.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() = %v, want nil", err)
	}
}

func TestPreflightFailsClosedOnUnsetEnvRef(t *testing.T) {
	os.Unsetenv("GOOBERS_TEST_PREFLIGHT_MISSING")
	p := preflightResolver(t, []TokenRef{{Name: "github-issues", Env: "GOOBERS_TEST_PREFLIGHT_MISSING"}})

	err := p.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight() = nil, want error")
	}
	if !errors.Is(err, ErrPreflight) {
		t.Fatalf("errors.Is(err, ErrPreflight) = false for %v", err)
	}
	for _, want := range []string{"github-issues", "GOOBERS_TEST_PREFLIGHT_MISSING", "env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}

func TestPreflightFailsClosedOnEmptyEnvRef(t *testing.T) {
	t.Setenv("GOOBERS_TEST_PREFLIGHT_EMPTY", "   ")
	p := preflightResolver(t, []TokenRef{{Name: "empty-ref", Env: "GOOBERS_TEST_PREFLIGHT_EMPTY"}})

	err := p.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight() = nil, want error")
	}
	if !errors.Is(err, ErrTokenRefEmpty) {
		t.Fatalf("errors.Is(err, ErrTokenRefEmpty) = false for %v", err)
	}
}

func TestPreflightReportsEveryFailureAtOnce(t *testing.T) {
	os.Unsetenv("GOOBERS_TEST_PREFLIGHT_A")
	os.Unsetenv("GOOBERS_TEST_PREFLIGHT_B")
	t.Setenv("GOOBERS_TEST_PREFLIGHT_C", "fine")

	p := preflightResolver(t, []TokenRef{
		{Name: "ref-a", Env: "GOOBERS_TEST_PREFLIGHT_A"},
		{Name: "ref-b", Env: "GOOBERS_TEST_PREFLIGHT_B"},
		{Name: "ref-c", Env: "GOOBERS_TEST_PREFLIGHT_C"},
	})

	err := p.Preflight(context.Background())
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("errors.As(err, *PreflightError) = false for %v", err)
	}
	if len(pe.Problems) != 2 {
		t.Fatalf("Problems = %d, want 2 (%v)", len(pe.Problems), pe.Problems)
	}
	if pe.Checked != 3 {
		t.Errorf("Checked = %d, want 3", pe.Checked)
	}
	// Ordered by ref name so repeated startups produce a stable diagnostic.
	if pe.Problems[0].Ref != "ref-a" || pe.Problems[1].Ref != "ref-b" {
		t.Errorf("Problems order = %q, %q, want ref-a, ref-b", pe.Problems[0].Ref, pe.Problems[1].Ref)
	}
}

func TestPreflightSkipsSourcesWithSideEffects(t *testing.T) {
	// Keychain prompts and store refs call the network, so neither is
	// exercised at startup: an instance built only from them preflights clean.
	resolver, err := NewResolverWithStores(
		[]TokenRef{
			{Name: "keychain-ref", Keychain: "goobers-test-service"},
			{Name: "store-ref", Store: "vault/secret"},
		},
		stubStoreResolver(func(context.Context, string) (string, error) {
			t.Fatal("store resolver must not be called during preflight")
			return "", nil
		}),
	)
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	p, ok := resolver.(Preflighter)
	if !ok {
		t.Fatalf("resolver does not implement Preflighter")
	}
	if err := p.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() = %v, want nil", err)
	}
}

func TestPreflightSkipsDynamicMintingSources(t *testing.T) {
	// A minting source consumes a provider rate budget per call; preflight
	// must not spend one on every daemon start.
	resolver, err := NewResolverWithSources(nil, map[string]ResolveFunc{
		"app-ref": func(context.Context) (string, error) {
			t.Fatal("dynamic source must not be minted during preflight")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	p, ok := resolver.(Preflighter)
	if !ok {
		t.Fatalf("resolver does not implement Preflighter")
	}
	if err := p.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() = %v, want nil", err)
	}
}

func TestPreflightNeverReportsSecretValues(t *testing.T) {
	const secret = "ghp_supersecretvalue"
	t.Setenv("GOOBERS_TEST_PREFLIGHT_SET", secret)
	os.Unsetenv("GOOBERS_TEST_PREFLIGHT_UNSET")

	p := preflightResolver(t, []TokenRef{
		{Name: "good", Env: "GOOBERS_TEST_PREFLIGHT_SET"},
		{Name: "bad", Env: "GOOBERS_TEST_PREFLIGHT_UNSET"},
	})
	err := p.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight() = nil, want error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("preflight error leaked a secret value: %q", err.Error())
	}
}

func TestPreflightHonorsContextCancellation(t *testing.T) {
	os.Unsetenv("GOOBERS_TEST_PREFLIGHT_CANCELLED")
	p := preflightResolver(t, []TokenRef{{Name: "ref", Env: "GOOBERS_TEST_PREFLIGHT_CANCELLED"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Preflight(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Preflight() = %v, want context.Canceled", err)
	}
}

type stubStoreResolver func(context.Context, string) (string, error)

func (f stubStoreResolver) FetchSecret(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}
