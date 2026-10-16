package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

type stageMutationObservingClient struct {
	providers.HTTPClient
	notices atomic.Int32
}

func (c *stageMutationObservingClient) ObserveProviderMutation(context.Context, providers.ProviderKind) {
	c.notices.Add(1)
}

func TestUncachedStageWriteInvalidatesSharedReadWithoutCachingItsReads(t *testing.T) {
	root := initDemo(t)
	t.Setenv(executor.GaggleEnvVar, "own")
	t.Setenv(executor.ProviderReadBindingEnvVar, "automation")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "generation-1")
	t.Setenv(providersnapshot.EnvVar, "one-evaluation")
	var reads, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			reads.Add(1)
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			writes.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":81,"body":"prepared"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	transport := &stageMutationObservingClient{HTTPClient: server.Client()}
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append([]func(*providers.GitHubProvider){providers.WithHTTPClient(transport), func(p *providers.GitHubProvider) { p.BaseURL = server.URL }}, opts...)...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	reader := newCachedGitHubProvider(root, "token")
	writerProvider, err := newProviderForStage(root, repo, false, withStageProviderCapability(capability.GitHubPRWrite), withStageProviderToken("token"))
	if err != nil {
		t.Fatal(err)
	}
	writer := writerProvider.(*providers.GitHubProvider)
	read := func(p *providers.GitHubProvider) {
		t.Helper()
		if _, err := p.ListOpenPullRequests(t.Context(), repo); err != nil {
			t.Fatal(err)
		}
	}
	read(reader)
	read(reader)
	if reads.Load() != 1 {
		t.Fatal("shared reader did not reuse snapshot", reads.Load())
	}
	read(writer)
	read(writer)
	if reads.Load() != 3 {
		t.Fatal("uncached stage reads were unexpectedly reused", reads.Load())
	}
	if _, err := writer.CreateWorkItemComment(t.Context(), repo, "7", "prepared"); err != nil {
		t.Fatal(err)
	}
	read(reader)
	read(reader)
	if transport.notices.Load() != 1 {
		t.Fatal("lost original transport mutation observer", transport.notices.Load())
	}
	if writes.Load() != 1 || reads.Load() != 4 {
		t.Fatalf("confirmed uncached stage write left shared reads stale: writes=%d reads=%d", writes.Load(), reads.Load())
	}
}

func TestStageProviderCachePreservesGitDiscoveryTLS(t *testing.T) {
	root := initDemo(t)
	t.Setenv(executor.GaggleEnvVar, "own")
	t.Setenv(executor.ProviderReadBindingEnvVar, "automation")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "generation-1")
	var discovery atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/repos/acme/app":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		case "/acme/app.git/info/refs":
			discovery.Add(1)
			user, token, ok := r.BasicAuth()
			if !ok || user != "x-access-token" || token != "token" {
				t.Error("Git discovery lost its selected credential")
			}
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			_, _ = io.WriteString(w, "001f# service=git-receive-pack\n0000")
		case "/api/v3/repos/acme/app/rules/branches/goobers/run-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append([]func(*providers.GitHubProvider){providers.WithHTTPClient(server.Client()), func(p *providers.GitHubProvider) { p.BaseURL = server.URL + "/api/v3" }}, opts...)...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	for _, cached := range []bool{false, true} {
		p, err := newProviderForStage(root, repo, cached, withStageProviderCapability(capability.GitHubPRWrite), withStageProviderToken("token"))
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.(*providers.GitHubProvider).PreflightRepositoryWrite(t.Context(), repo, "goobers/run-1")
		if err != nil || !result.OK {
			t.Errorf("cached=%v: configured TLS transport lost: %+v, %v", cached, result, err)
		}
	}
	if discovery.Load() != 2 {
		t.Errorf("Git discovery count=%d, want two live probes", discovery.Load())
	}
}
