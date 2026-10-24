package terminalwriter

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func configuredBackends(t *testing.T, applied AppliedConfig) *ConfiguredBackends {
	t.Helper()
	backends, err := NewConfiguredBackends(applied, journal.NewRegistryScrubber(), func(*instance.Config) (credentials.StoreResolver, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	return backends
}

func TestConfiguredBackendsWaitForAnAppliedConfig(t *testing.T) {
	backends := configuredBackends(t, func() (*instance.Config, string, bool) { return nil, "", false })
	if _, err := backends.For(t.Context(), binding("run-1"), workspaceRepo, string(capability.GitHubBranchDelete)); !errors.Is(err, ErrNoAppliedConfig) {
		t.Fatalf("before any config applied: %v", err)
	}
}

func TestConfiguredBackendsRefuseAnUnconfiguredProject(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "elsewhere", Token: instance.TokenRef{Env: "TERMINALWRITER_TEST_TOKEN"}}}}
	backends := configuredBackends(t, func() (*instance.Config, string, bool) { return cfg, "d1", true })
	if _, err := backends.For(t.Context(), binding("run-1"), workspaceRepo, string(capability.GitHubBranchDelete)); err == nil {
		t.Fatal("a gaggle project absent from the applied config must not borrow another repository's credentials")
	}
}

func TestConfiguredBackendsRebuildCredentialsWhenTheAppliedDigestMoves(t *testing.T) {
	t.Setenv("TERMINALWRITER_TEST_TOKEN", "token-value")
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "service", Token: instance.TokenRef{Env: "TERMINALWRITER_TEST_TOKEN"}}}}
	digest := "d1"
	backends := configuredBackends(t, func() (*instance.Config, string, bool) { return cfg, digest, true })
	injector := func() *credentials.Injector {
		t.Helper()
		backend, err := backends.For(t.Context(), binding("run-1"), workspaceRepo, string(capability.GitHubBranchDelete))
		if err != nil {
			t.Fatal(err)
		}
		if backend.Kind() != providers.ProviderGitHub {
			t.Fatalf("backend kind %s", backend.Kind())
		}
		if len(backends.injectors) != 1 {
			t.Fatalf("credentials cached for %d scopes, want 1", len(backends.injectors))
		}
		for _, scoped := range backends.injectors {
			return scoped.injector
		}
		return nil
	}
	first := injector()
	if again := injector(); again != first {
		t.Fatal("credentials rebuilt while the applied digest was unchanged")
	}
	digest = "d2"
	if moved := injector(); moved == first {
		t.Fatal("credentials survived a new applied configuration")
	}
}
