package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestChildPublicationADOUsesDeliveredSchemeExpiryAndNoMutationRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer host-only" {
			t.Errorf("wrong delivered authorization scheme: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	previous := newADOProviderForStage
	t.Cleanup(func() { newADOProviderForStage = previous })
	newADOProviderForStage = func(repo providers.RepositoryRef, source providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(repo.Owner, repo.Project, "", providers.WithADOCredentialSource(source), func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	// The host environment deliberately disagrees with the brokered scheme.
	t.Setenv(executor.RepoAuthSchemeEnvVar, "basic")
	pod := &childStagePod{childPodFactory: childPodFactory{service: &daemonCredentialService{layout: instance.NewLayout(t.TempDir()), shared: journal.NewRegistryScrubber()}}, identity: journal.RunIdentity{RunID: "child", Child: &journal.ChildLineage{ParentRunID: "parent", StageOccurrence: "stage"}}}
	target := childpublication.Target{Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}, Remote: "https://dev.azure.com/org/project/_git/repo"}
	expires := time.Now().Add(time.Minute)
	credential := httpapi.MintedCredential{Value: "host-only", ExpiresAt: &expires}
	pub, err := pod.publicationProvider(target, "provider:pr:write", credential, "bearer")
	if err != nil {
		t.Fatal(err)
	}
	_, err = pub.PRs.CreatePullRequest(t.Context(), providers.PullRequestRequest{Repository: target.Repository, Head: "child", Base: "main", Title: "title"})
	if err == nil || calls.Load() != 1 {
		t.Fatal("publication retried a rate-limited mutation", calls.Load(), err)
	}
	expires = time.Now().Add(-time.Minute)
	pub, err = pod.publicationProvider(target, "provider:pr:write", credential, "bearer")
	if err == nil {
		_, err = pub.PRs.CreatePullRequest(t.Context(), providers.PullRequestRequest{Repository: target.Repository, Head: "child", Base: "main", Title: "title"})
	}
	if err == nil || calls.Load() != 1 {
		t.Fatal("expired publication credential reached provider", calls.Load(), err)
	}
}
