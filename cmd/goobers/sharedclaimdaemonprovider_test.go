package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

func TestSharedStoreScrubbingPreservesCoordinationClassification(t *testing.T) {
	registry, _ := journal.DefaultScrubber()
	registry.Register([]byte("private-claim-token"))
	store := scrubbedSharedClaimStore{registrar: registry}
	for _, sentinel := range []error{sharedclaim.ErrConflict, sharedclaim.ErrHeld, sharedclaim.ErrNotOwner} {
		err := store.scrubError(fmt.Errorf("private-claim-token: %w", sentinel))
		if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "private-claim-token") {
			t.Fatalf("scrubbed error lost classification or exposed credential: %v", err)
		}
	}
}

func TestDaemonSharedClaimStoreUsesExactConfiguredRepository(t *testing.T) {
	const token = "daemon-owned-claim-token"
	t.Setenv("SHARED_CLAIM_DAEMON_TEST_TOKEN", token)
	t.Setenv("GH_TOKEN", "stage-token-must-not-be-used")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("provider did not use the configured daemon credential")
		}
		if !strings.HasPrefix(r.URL.Path, "/repos/acme/app/git/ref/") {
			t.Errorf("unexpected provider path: %s", r.URL.Path)
		}
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	registry, _ := journal.DefaultScrubber()
	configured := instance.RepoRef{Provider: "github", BaseURL: server.URL, Owner: "acme", Name: "app", Token: instance.TokenRef{Env: "SHARED_CLAIM_DAEMON_TEST_TOKEN"}}
	cfg := &instance.Config{Repos: []instance.RepoRef{configured}}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, URL: server.URL, Owner: "acme", Name: "app"}
	store, err := daemonSharedClaimStore(t.Context(), cfg, repo, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(t.Context(), "42"); err != nil || calls != 1 {
		t.Fatalf("read: %v calls=%d", err, calls)
	}
	foreign := repo
	foreign.URL = "https://another-host.invalid"
	if _, err := daemonSharedClaimStore(t.Context(), cfg, foreign, registry, nil); err == nil || calls != 1 {
		t.Fatal("another host acquired this repository's credentials")
	}
	cfg.Repos = append(cfg.Repos, configured)
	if _, err := daemonSharedClaimStore(t.Context(), cfg, repo, registry, nil); err == nil || calls != 1 {
		t.Fatal("ambiguous repository credentials were accepted")
	}
}
