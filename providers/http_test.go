package providers

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBasicAuth(t *testing.T) {
	const want = "Basic YnVpbGQtYWdlbnQ6YWRvLXBhdC0wMTIzNDU2Nzg5"
	if got := basicAuth("build-agent", "ado-pat-0123456789"); got != want {
		t.Fatalf("basicAuth() = %q, want %q", got, want)
	}
}

func TestADOProviderRegistersBasicAuthCredential(t *testing.T) {
	const token = "ado-pat-0123456789"
	reg := journal.NewRegistryScrubber()
	NewADOProvider("org", "project", token,
		func(p *ADOProvider) { p.Username = "build-agent" },
		WithADOSecretRegistrar(reg),
	)

	encoded := strings.TrimPrefix(basicAuth("build-agent", token), "Basic ")
	for _, credential := range []string{token, encoded} {
		got := reg.Scrub([]byte("captured credential: " + credential))
		if bytes.Contains(got, []byte(credential)) || !bytes.Contains(got, []byte(journal.Redacted)) {
			t.Fatalf("registered credential was not redacted: %q", got)
		}
	}
}

func TestDefaultProviderHTTPClientHasTimeout(t *testing.T) {
	client, ok := httpClientOrDefault(nil).(*http.Client)
	if !ok {
		t.Fatalf("default client type = %T, want *http.Client", httpClientOrDefault(nil))
	}
	if client.Timeout != defaultProviderHTTPTimeout {
		t.Fatalf("default client timeout = %s, want %s", client.Timeout, defaultProviderHTTPTimeout)
	}
}

func TestProviderHTTPClientBoundsStalledEndpointRetries(t *testing.T) {
	requests := make(chan struct{}, 3)
	client := newProviderHTTPClient(20 * time.Millisecond)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})

	const retries = 2
	provider := NewGitHubProvider("token",
		func(p *GitHubProvider) {
			p.Client = client
			p.sleep = func(context.Context, time.Duration) error { return nil }
		},
		WithMaxTransientRetries(retries),
	)

	start := time.Now()
	_, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Owner: "owner", Name: "repo"},
	})
	if err == nil {
		t.Fatal("ListWorkItems() error = nil, want timeout")
	}
	// The real "retries are bounded" invariant is the attempt count below:
	// exactly retries+1 requests, each terminated by the client timeout. The
	// elapsed check is only a coarse backstop against an *unbounded* regression
	// (which would fill the buffered channel and hang) failing faster than the
	// package test timeout — NOT a tight timing assertion. The former 500ms
	// ceiling was ~8x the ~60ms nominal (3 x 20ms) and still tripped on the
	// contended macOS hosted runner (#1195/#1239 flake); a whole-second budget
	// keeps the hang backstop while giving CI scheduling jitter ample headroom.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stalled request took %s — retries appear unbounded, not capped by the client timeout", elapsed)
	}
	if got, want := len(requests), retries+1; got != want {
		t.Fatalf("request attempts = %d, want %d", got, want)
	}
}
