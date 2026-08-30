package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// recordingForge stands in for api.github.com. It records every path the
// provider actually requests and answers GET /user exactly as GitHub answers it
// for a GitHub App installation token: 403 "Resource not accessible by
// integration". A provider that still needs /user to learn its own login cannot
// hide it here.
type recordingForge struct {
	mu     sync.Mutex
	paths  []string
	server *httptest.Server
	// userLogin, when non-empty, makes GET /user succeed with that login (the
	// PAT case); otherwise /user 403s the way an installation token sees it.
	userLogin string
}

func newRecordingForge(t *testing.T, userLogin string) *recordingForge {
	t.Helper()
	forge := &recordingForge{userLogin: userLogin}
	forge.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forge.mu.Lock()
		forge.paths = append(forge.paths, r.URL.Path)
		forge.mu.Unlock()
		if r.URL.Path == "/user" && forge.userLogin != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"login":"` + forge.userLogin + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	t.Cleanup(forge.server.Close)
	return forge
}

func (f *recordingForge) requestedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *recordingForge) requested(path string) bool {
	for _, got := range f.requestedPaths() {
		if got == path {
			return true
		}
	}
	return false
}

// declareGitHubAppAuth rewrites root's config so its GitHub repo authenticates
// as a GitHub App with the given slug — the shape the live instance ships.
func declareGitHubAppAuth(t *testing.T, root, slug string) providers.RepositoryRef {
	t.Helper()
	configPath := instance.NewLayout(root).ConfigFile()
	cfg, err := instance.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	ref := githubRepoRefFromConfig(t, cfg)
	for i := range cfg.Repos {
		if cfg.Repos[i].Provider != "github" {
			continue
		}
		cfg.Repos[i].Token = instance.TokenRef{}
		cfg.Repos[i].Auth = &instance.RepoAuthConfig{
			Kind:           instance.GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "42",
			PrivateKey:     &instance.TokenRef{File: "/run/secrets/goobers-app.pem"},
			Slug:           slug,
		}
	}
	if err := instance.WriteConfig(configPath, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	return ref
}

func githubRepoRefFromConfig(t *testing.T, cfg *instance.Config) providers.RepositoryRef {
	t.Helper()
	for _, repo := range cfg.Repos {
		if repo.Provider == "github" {
			return providers.RepositoryRef{
				Provider: providers.ProviderGitHub,
				Owner:    repo.Owner,
				Name:     repo.Name,
			}
		}
	}
	t.Fatalf("config declares no github repo")
	return providers.RepositoryRef{}
}

// TestApplyVerdictProviderUsesConfiguredAppLoginWithoutUserRequest is the #3885
// regression: apply-verdict's REAL provider constructor, under App auth, must
// resolve its identity from repos[].auth.slug and never touch GET /user.
//
// Before the fix newApplyVerdictProviderForRepo called newCachedGitHubProvider
// directly, bypassing newGitHubProviderForStage and therefore
// providers.WithConfiguredLogin (#3343/#3344), so AuthenticatedLogin fell
// through to /user, got 403 "Resource not accessible by integration", and
// tripped the scheduler's per-workflow auth circuit for merge-review.
func TestApplyVerdictProviderUsesConfiguredAppLoginWithoutUserRequest(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")

	forge := newRecordingForge(t, "")

	provider, err := newApplyVerdictProviderForRepo(root, repo)
	if err != nil {
		t.Fatalf("newApplyVerdictProviderForRepo: %v", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GitHubProvider", provider)
	}
	githubProvider.BaseURL = forge.server.URL

	login, err := githubProvider.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "goobersbot[bot]" {
		t.Fatalf("AuthenticatedLogin = %q, want %q", login, "goobersbot[bot]")
	}
	if paths := forge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("provider made %d API request(s) %v, want none — the configured App login must be answered locally", len(paths), paths)
	}
}

// TestApplyVerdictProviderKeepsUserLookupForPAT pins the other half of the
// contract: with no configured App slug (the PAT case) identity resolution must
// still go through GET /user.
func TestApplyVerdictProviderKeepsUserLookupForPAT(t *testing.T) {
	root := initDemo(t)
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := githubRepoRefFromConfig(t, cfg)
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "pat-token")

	forge := newRecordingForge(t, "pat-user")

	provider, err := newApplyVerdictProviderForRepo(root, repo)
	if err != nil {
		t.Fatalf("newApplyVerdictProviderForRepo: %v", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GitHubProvider", provider)
	}
	githubProvider.BaseURL = forge.server.URL

	login, err := githubProvider.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "pat-user" {
		t.Fatalf("AuthenticatedLogin = %q, want %q", login, "pat-user")
	}
	if !forge.requested("/user") {
		t.Fatalf("requested paths = %v, want a GET /user lookup", forge.requestedPaths())
	}
}

// TestApplyVerdictProviderRoutesThroughStageSeam proves the constructor reaches
// the registered stage factories — the seam #3343 fixed — and that each arm
// still asks for exactly the options it asked for before: PR-write capability
// everywhere, conditional-GET caching and no mutation recorder on GitHub, the
// kind="pr" recorder and no cache on Gitea, and a write-mode (not read-only)
// provider in every case.
func TestApplyVerdictProviderRoutesThroughStageSeam(t *testing.T) {
	for _, tc := range []struct {
		name             string
		kind             providers.ProviderKind
		repo             providers.RepositoryRef
		wantCached       bool
		wantMutationKind string
	}{
		{
			name:       "github",
			kind:       providers.ProviderGitHub,
			repo:       providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
			wantCached: true,
		},
		{
			name:             "gitea",
			kind:             providers.ProviderGitea,
			repo:             providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "acme", Name: "web"},
			wantMutationKind: "pr",
		},
		{
			name: "ado",
			kind: providers.ProviderADO,
			repo: providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "contoso", Project: "project", Name: "repo"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := stageProviderFactories[tc.kind]
			t.Cleanup(func() { stageProviderFactories[tc.kind] = previous })
			var got stageProviderConfig
			var called bool
			stageProviderFactories[tc.kind] = func(cfg stageProviderConfig) (providers.Provider, error) {
				got = cfg
				called = true
				return providers.NewGitHubProvider("stub"), nil
			}

			if _, err := newApplyVerdictProviderForRepo(t.TempDir(), tc.repo); err != nil {
				t.Fatalf("newApplyVerdictProviderForRepo: %v", err)
			}
			if !called {
				t.Fatalf("registered stage factory for %q was not called", tc.kind)
			}
			if got.capability != capability.ProviderPRWrite {
				t.Fatalf("capability = %q, want %q", got.capability, capability.ProviderPRWrite)
			}
			if got.readOnly {
				t.Fatalf("readOnly = true, want false")
			}
			if got.cached != tc.wantCached {
				t.Fatalf("cached = %v, want %v", got.cached, tc.wantCached)
			}
			if got.mutationKind != tc.wantMutationKind {
				t.Fatalf("mutationKind = %q, want %q", got.mutationKind, tc.wantMutationKind)
			}
			if got.openPR {
				t.Fatalf("openPR = true, want false")
			}
		})
	}
}

func TestApplyVerdictProviderRejectsUnsupportedProvider(t *testing.T) {
	_, err := newApplyVerdictProviderForRepo(t.TempDir(), providers.RepositoryRef{Provider: "unknown"})
	if err == nil || !strings.Contains(err.Error(), `apply-verdict does not support repository provider "unknown"`) {
		t.Fatalf("error = %v, want apply-verdict unsupported-provider error", err)
	}
}
