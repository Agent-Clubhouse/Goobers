package terminalwriter

import (
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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

func TestGiteaBranchDeleteIsRefusedNotRetried(t *testing.T) {
	t.Setenv("TERMINALWRITER_TEST_TOKEN", "token-value")
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "gitea", BaseURL: "https://gitea.example", Owner: "acme", Name: "service", Token: instance.TokenRef{Env: "TERMINALWRITER_TEST_TOKEN"}}}}
	bound := binding("run-1")
	bound.Project.Provider, bound.Project.BaseURL = "gitea", "https://gitea.example"
	bound.Workspace.Provider = providers.ProviderGitea
	backends := configuredBackends(t, func() (*instance.Config, string, bool) { return cfg, "d1", true })
	backend, err := backends.For(t.Context(), bound, bound.Workspace, string(capability.GitHubBranchDelete))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.DeleteBranch(t.Context(), providers.DeleteBranchRequest{Repository: bound.Workspace, Name: "goobers/run-1"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("gitea delete in the control role: %v, want ErrUnsupported", err)
	}
}

// TestCredentialsAreNarrowedToThePinnedHost: two repositories share owner/name
// on different hosts. Only the pinned host's entry may supply the token;
// matching on owner/name alone, or binding against the whole config, hands
// out the other host's token.
func TestCredentialsAreNarrowedToThePinnedHost(t *testing.T) {
	t.Setenv("TERMINALWRITER_TOKEN_A", "token-host-a")
	t.Setenv("TERMINALWRITER_TOKEN_B", "token-host-b")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "gitea", BaseURL: "https://a.example", Owner: "acme", Name: "service", Token: instance.TokenRef{Env: "TERMINALWRITER_TOKEN_A"}},
		{Provider: "gitea", BaseURL: "https://b.example", Owner: "acme", Name: "service", Token: instance.TokenRef{Env: "TERMINALWRITER_TOKEN_B"}},
	}}
	backends := configuredBackends(t, func() (*instance.Config, string, bool) { return cfg, "d1", true })
	bound := binding("run-1")
	bound.Project = apiv1.RepoRef{Provider: apiv1.ProviderGitea, BaseURL: "https://b.example", Owner: "acme", Name: "service"}
	bound.Workspace = repositoryRef(bound.Project)
	configured, err := configuredRepo(cfg, bound.Project)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := backends.credentials(cfg, "d1", bound, bound.Workspace, configured)
	if err != nil {
		t.Fatal(err)
	}
	set, err := scoped.injector.Materialize(t.Context(), []string{string(capability.GitHubBranchDelete)})
	if err != nil {
		t.Fatal(err)
	}
	token, err := set.For(string(capability.GitHubBranchDelete)).Token(t.Context())
	if err != nil || token != "token-host-b" {
		t.Fatalf("pinned host b resolved %q (%v), want host b's token", token, err)
	}
	// A project naming neither host's exact entry is refused, not guessed.
	bound.Project.BaseURL = "https://c.example"
	if _, err := configuredRepo(cfg, bound.Project); err == nil {
		t.Fatal("a project on an unconfigured host borrowed a configured repository")
	}
}
