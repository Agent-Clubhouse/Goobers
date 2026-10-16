package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

type providerAuthTestResolver map[string]string

func (r providerAuthTestResolver) Resolve(_ context.Context, ref string) (string, error) {
	return r[ref], nil
}

func TestProviderAuthGatesResolveWorkflowRepositoryCredential(t *testing.T) {
	var sawToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = server.URL })...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	gates := newProviderAuthGates(providerAuthTestResolver{"acme/web": "revoked-token"}, journal.NewRegistryScrubber(), "gen-1")
	repoRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	wf := &apiv1.Workflow{}
	if gate := gates.forWorkflow(&instance.Config{}, wf, repoRef); gate != nil {
		t.Fatalf("workflow without requireProviderAuthorization got gate %#v", gate)
	}
	wf.Spec.Readiness.RequireProviderAuthorization = true
	gate := gates.forWorkflow(&instance.Config{}, wf, repoRef)
	if gate == nil {
		t.Fatal("opted-in workflow got no gate")
	}
	status := gate.ProviderAuthStatus(context.Background(), time.Now())
	if status.Healthy || status.Repository != "acme/web" || status.Provider != providers.ProviderGitHub {
		t.Fatalf("status = %+v, want an unhealthy acme/web GitHub check", status)
	}
	if status.Code != localscheduler.ProviderAuthRejected || sawToken != "Bearer revoked-token" {
		t.Fatalf("status code = %s with auth %q, want the resolved credential rejected", status.Code, sawToken)
	}
}
