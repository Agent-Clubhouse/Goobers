package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestSharedVisibilityRegistrationRejectsCredentialBearingURLs(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	for _, address := range []string{"https://user:secret@example.com/api", "https://example.com/api?token=secret", "https://example.com/api#secret", "file:///tmp/credentials", "relative-path"} {
		repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo", URL: address}
		if err := registerSharedVisibilityRepository(t.Context(), layout, repo); err == nil {
			t.Fatalf("credential-bearing or invalid URL persisted: %q", address)
		}
	}
	if _, err := os.Stat(layout.SchedulerDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid registration touched disk: %v", err)
	}
}

func TestSharedVisibilityRegistrySurvivesConcurrentRegistration(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo"}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- registerSharedVisibilityRepository(t.Context(), layout, repo) }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Read from a fresh layout: no run journal or claim ledger is present.
	repos, err := sharedVisibilityRepositories(instance.NewLayout(layout.Root))
	if err != nil || len(repos) != 1 || repos[0] != repo {
		t.Fatalf("registration did not survive: %+v %v", repos, err)
	}
	if err := os.WriteFile(filepath.Join(layout.SchedulerDir(), sharedRepositoryPrefix+"corrupt.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	repos, err = sharedVisibilityRepositories(layout)
	if err == nil || len(repos) != 1 || repos[0] != repo {
		t.Fatalf("corrupt registration hid valid repository: %+v %v", repos, err)
	}
}

func TestSharedVisibilitySweepBoundsAndRotatesRepositories(t *testing.T) {
	var repos []providers.RepositoryRef
	for i := range 6 {
		repos = append(repos, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: fmt.Sprintf("repo-%d", i)})
	}
	sweep := sharedVisibilitySweep{}
	var visited []string
	visit := func(ctx context.Context, repo providers.RepositoryRef, cursor string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("repository scan has no bound")
		}
		visited = append(visited, repo.Name)
		return cursor + "next", errors.New("retry required")
	}
	if err := sweep.reconcile(t.Context(), repos, visit); err == nil || len(visited) != 4 {
		t.Fatalf("first batch: %v %v", visited, err)
	}
	if err := sweep.reconcile(t.Context(), repos, visit); err == nil || len(visited) != 6 || sweep.repositoryCursor != "" {
		t.Fatalf("failed repositories starved later ones: %v %v", visited, err)
	}
	if err := sweep.reconcile(t.Context(), repos, visit); err == nil || len(visited) != 10 {
		t.Fatalf("new sweep did not retry: %v %v", visited, err)
	}
	if sweep.cursors[repos[0].CanonicalKey()] != "nextnext" {
		t.Fatal("item continuation was discarded")
	}
}

func TestSharedVisibilityDaemonFindsRegistrationWithoutLocalClaim(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	t.Setenv("SHARED_RETRY_TEST_TOKEN", "daemon-retry-token")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer daemon-retry-token" || r.URL.Path != "/repos/acme/repo/git/matching-refs/heads/goobers-shared-claims/" {
			t.Errorf("unexpected retry request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []instance.RepoRef{{Provider: "github", BaseURL: server.URL, Owner: "acme", Name: "repo", Token: instance.TokenRef{Env: "SHARED_RETRY_TEST_TOKEN"}}}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	sweep := sharedVisibilitySweep{layout: layout}
	if err := sweep.run(t.Context()); err != nil || calls != 0 {
		t.Fatalf("local-only instance contacted provider: %v %d", err, calls)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, URL: server.URL, Owner: "acme", Name: "repo"}
	if err := registerSharedVisibilityRepository(t.Context(), layout, repo); err != nil {
		t.Fatal(err)
	}
	if err := sweep.run(t.Context()); err != nil || calls != 1 {
		t.Fatalf("empty-ledger retry did not run: %v %d", err, calls)
	}
}
