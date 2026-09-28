package credentials

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type identityRoundTripper func(*http.Request) (*http.Response, error)

func (f identityRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// #5596: an exhausted-quota 403 while verifying a GitHub CLI credential was
// indistinguishable from a bad token, so a config reload rejected by it was
// never retried and a daemon start failed outright.
func TestGitHubCLIIdentityClassifiesTransientProviderFailures(t *testing.T) {
	oldCommand := githubCLICommand
	oldTransport := http.DefaultClient.Transport
	t.Cleanup(func() {
		githubCLICommand = oldCommand
		http.DefaultClient.Transport = oldTransport
	})
	githubCLICommand = func(context.Context, string, string) (string, error) { return "tok", nil }

	for _, tc := range []struct {
		name      string
		status    int
		header    http.Header
		netErr    error
		transient bool
	}{
		{name: "quota exhausted 403", status: http.StatusForbidden, header: http.Header{"X-Ratelimit-Remaining": {"0"}}, transient: true},
		{name: "secondary rate limit 403", status: http.StatusForbidden, header: http.Header{"Retry-After": {"60"}}, transient: true},
		{name: "429", status: http.StatusTooManyRequests, transient: true},
		{name: "502", status: http.StatusBadGateway, transient: true},
		{name: "network", netErr: errors.New("dial tcp: connection refused"), transient: true},
		{name: "plain 403", status: http.StatusForbidden, header: http.Header{"X-Ratelimit-Remaining": {"4999"}}},
		{name: "401", status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			http.DefaultClient.Transport = identityRoundTripper(func(req *http.Request) (*http.Response, error) {
				if tc.netErr != nil {
					return nil, tc.netErr
				}
				header := tc.header
				if header == nil {
					header = http.Header{}
				}
				return &http.Response{
					StatusCode: tc.status, Status: http.StatusText(tc.status), Header: header,
					Body: io.NopCloser(strings.NewReader("")), Request: req,
				}, nil
			})
			_, err := NewResolver([]TokenRef{{Name: "gh", GitHubCLI: &GitHubCLIRef{Hostname: "github.com", User: "alice"}}})
			if err == nil {
				t.Fatal("NewResolver: want verification error, got nil")
			}
			if got := errors.Is(err, ErrTransientProvider); got != tc.transient {
				t.Fatalf("errors.Is(%v, ErrTransientProvider) = %t, want %t", err, got, tc.transient)
			}
			if !strings.Contains(err.Error(), "verify GitHub identity") || strings.Contains(err.Error(), ErrTransientProvider.Error()) {
				t.Fatalf("error = %q, want the unchanged verification message", err)
			}
		})
	}
}
