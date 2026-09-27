package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testReceivePackAdvertisement = "001f# service=git-receive-pack\n00000000"

func TestPreflightRepositoryWriteGitDiscovery(t *testing.T) {
	tests := []struct {
		name        string
		rolesAbsent bool
		status      int
		contentType string
		body        string
		rulesStatus int
		blocking    bool
		want        RepoWriteFailureCapability
		wantRules   bool
	}{
		{name: "write App with false user roles", status: 200, contentType: "application/x-git-receive-pack-advertisement", body: testReceivePackAdvertisement, wantRules: true},
		{name: "write App with absent roles", rolesAbsent: true, status: 200, contentType: "application/x-git-receive-pack-advertisement", body: testReceivePackAdvertisement, wantRules: true},
		{name: "read-only App", status: 403, want: RepoWriteFailureNoPushPermission},
		{name: "expired or invalid token", status: 401, want: RepoWriteFailureUnauthorized},
		{name: "wrong repository", status: 404, want: RepoWriteFailureUnauthorized},
		{name: "rate limited", status: 429, want: RepoWriteFailurePolicyIntrospectionUnavailable},
		{name: "server unavailable", status: 503, want: RepoWriteFailurePolicyIntrospectionUnavailable},
		{name: "HTML is not permission", status: 200, contentType: "text/html", body: testReceivePackAdvertisement, want: RepoWriteFailurePolicyIntrospectionUnavailable},
		{name: "fetch service is not push", status: 200, contentType: "application/x-git-upload-pack-advertisement", body: "001e# service=git-upload-pack\n0000", want: RepoWriteFailurePolicyIntrospectionUnavailable},
		{name: "wrong service announcement", status: 200, contentType: "application/x-git-receive-pack-advertisement", body: "001e# service=git-upload-pack\n0000", want: RepoWriteFailurePolicyIntrospectionUnavailable},
		{name: "truncated announcement", status: 200, contentType: "application/x-git-receive-pack-advertisement", body: "001f# service=git-receive-pack\n", want: RepoWriteFailurePolicyIntrospectionUnavailable},
		{name: "write still checks branch policy", status: 200, contentType: "application/x-git-receive-pack-advertisement", body: testReceivePackAdvertisement, blocking: true, want: RepoWriteFailureBranchPolicy, wantRules: true},
		{name: "write cannot skip unavailable rules", status: 200, contentType: "application/x-git-receive-pack-advertisement", body: testReceivePackAdvertisement, rulesStatus: 403, want: RepoWriteFailurePolicyIntrospectionUnavailable, wantRules: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gitReads, rulesReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodGet)
				switch r.URL.Path {
				case "/repos/acme/app":
					if tc.rolesAbsent {
						writeJSON(t, w, map[string]any{})
					} else {
						writeJSON(t, w, map[string]any{"permissions": map[string]bool{"push": false}})
					}
				case "/acme/app.git/info/refs":
					gitReads.Add(1)
					user, token, ok := r.BasicAuth()
					if !ok || user != "x-access-token" || token != "opaque-installation-token" || r.URL.RawQuery != "service=git-receive-pack" || r.ContentLength != 0 {
						t.Error("Git probe must be an authenticated, bodyless receive-pack discovery GET")
					}
					w.Header().Set("Content-Type", tc.contentType)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				case "/repos/acme/app/rules/branches/goobers/run-1":
					rulesReads.Add(1)
					if tc.rulesStatus != 0 {
						w.WriteHeader(tc.rulesStatus)
					} else if tc.blocking {
						writeJSON(t, w, []map[string]any{{"type": "branch_name_pattern", "parameters": map[string]any{"operator": "starts_with", "pattern": "goobers/"}}})
					} else {
						writeJSON(t, w, []any{})
					}
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			provider := NewGitHubProvider("opaque-installation-token", func(p *GitHubProvider) { p.BaseURL = server.URL })
			result, err := provider.PreflightRepositoryWrite(context.Background(), RepositoryRef{Owner: "acme", Name: "app", URL: "https://untrusted.invalid/other/repo.git"}, "goobers/run-1")
			if err != nil || result.FailureCapability != tc.want || result.OK != (tc.want == "") {
				t.Fatalf("result = %+v, err = %v, want %q", result, err, tc.want)
			}
			if gitReads.Load() != 1 || (rulesReads.Load() == 1) != tc.wantRules {
				t.Fatalf("reads: Git=%d rules=%d", gitReads.Load(), rulesReads.Load())
			}
		})
	}
}

func TestGitPreflightRefusesRedirects(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/acme/app.git/info/refs" {
			http.Redirect(w, r, "/other/repository", http.StatusMovedPermanently)
			return
		}
		redirected.Add(1)
	}))
	defer server.Close()
	provider := NewGitHubProvider("private-token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.preflightGitReceivePack(context.Background(), RepositoryRef{Owner: "acme", Name: "app"})
	if err != nil || result.FailureCapability != RepoWriteFailurePolicyIntrospectionUnavailable || redirected.Load() != 0 {
		t.Fatalf("result=%+v err=%v redirected=%d", result, err, redirected.Load())
	}
}

func TestGitPreflightBoundsAdvertisementRead(t *testing.T) {
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = w.Write([]byte(testReceivePackAdvertisement))
		w.(http.Flusher).Flush()
		// A very large or endless ref list must not be read into memory or
		// hold the preflight open after the fixed-size service announcement.
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := provider.preflightGitReceivePack(ctx, RepositoryRef{Owner: "acme", Name: "app"})
	if err != nil || !result.OK {
		t.Fatalf("bounded read: %+v, %v", result, err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("discovery body was not closed")
	}
}

func TestGitPreflightUsesConfiguredTLSTransport(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = w.Write([]byte(testReceivePackAdvertisement))
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", WithHTTPClient(server.Client()), func(p *GitHubProvider) { p.BaseURL = server.URL + "/api/v3" })
	result, err := provider.preflightGitReceivePack(context.Background(), RepositoryRef{Owner: "acme", Name: "app"})
	if err != nil || !result.OK {
		t.Fatalf("configured TLS transport: %+v, %v", result, err)
	}
}

func TestGitPreflightCancellationAndMissingCredential(t *testing.T) {
	provider := NewGitHubProvider("", func(p *GitHubProvider) { p.BaseURL = "http://127.0.0.1:1" })
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	result, err := provider.preflightGitReceivePack(context.Background(), repo)
	if err != nil || result.FailureCapability != RepoWriteFailureUnauthorized {
		t.Fatalf("missing credential: %+v, %v", result, err)
	}
	provider.Token = "private-token"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provider.preflightGitReceivePack(ctx, repo)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
	result, err = provider.preflightGitReceivePack(context.Background(), repo)
	if err != nil || result.FailureCapability != RepoWriteFailurePolicyIntrospectionUnavailable || strings.Contains(result.Detail, provider.Token) {
		t.Fatalf("transport failure must be inconclusive and sanitized: %+v, %v", result, err)
	}
}

func TestGitHubReceivePackEndpoint(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://api.github.com", "https://github.com/acme/app.git/info/refs?service=git-receive-pack"},
		{"https://api.octocorp.ghe.com", "https://octocorp.ghe.com/acme/app.git/info/refs?service=git-receive-pack"},
		{"https://github.example/api/v3/", "https://github.example/acme/app.git/info/refs?service=git-receive-pack"},
		{"https://github.example/prefix/api/v3", "https://github.example/prefix/acme/app.git/info/refs?service=git-receive-pack"},
		{"http://127.0.0.1:1234", "http://127.0.0.1:1234/acme/app.git/info/refs?service=git-receive-pack"},
		{"http://github.example/api/v3", ""},
		{"https://user:secret@api.github.com", ""},
		{"https://api.github.com?query=unexpected", ""},
		{"https://api.github.com#unexpected", ""},
		{"https://github.example/unknown", ""},
		{"not a URL", ""},
	} {
		t.Run(tc.base, func(t *testing.T) {
			got, err := githubReceivePackEndpoint(tc.base, RepositoryRef{Owner: "acme", Name: "app"})
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("endpoint=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"", ".", "..", "other/repo", "other\\repo", "repo?x", "repo#x", "%2e%2e"} {
		for _, repo := range []RepositoryRef{{Owner: bad, Name: "app"}, {Owner: "acme", Name: bad}} {
			if _, err := githubReceivePackEndpoint("https://api.github.com", repo); err == nil {
				t.Fatalf("accepted invalid repository segment: %+v", repo)
			}
		}
	}
}
