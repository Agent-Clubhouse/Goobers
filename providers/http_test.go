package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// TestIsMergeConflictError pins issue #1751's classification constraint: only
// a confirmed merge-conflict response may be reclassified from an
// infrastructure failure into a business refusal. GitHub answers the merge
// endpoint with 405 for several unrelated policy refusals, so the status code
// alone must never be sufficient — misclassifying one of those would let
// record-merge-refusal demote a lander for a reason that has nothing to do
// with conflicts.
func TestIsMergeConflictError(t *testing.T) {
	respErr := func(status int, body string) error {
		return &providerResponseError{
			method:     http.MethodPut,
			endpoint:   "https://api.github.com/repos/o/r/pulls/9/merge",
			statusCode: status,
			body:       body,
		}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"github not-mergeable 405", respErr(http.StatusMethodNotAllowed, `{"message":"Pull Request is not mergeable"}`), true},
		{"explicit merge conflict wording", respErr(http.StatusMethodNotAllowed, `{"message":"merge conflict between base and head"}`), true},
		{"wrapped still detected", fmt.Errorf("merge pull request: %w", respErr(http.StatusMethodNotAllowed, `{"message":"Pull Request is not mergeable"}`)), true},
		{"ruleset violation 405 is not a conflict", respErr(http.StatusMethodNotAllowed, `{"message":"Repository rule violations found\n\nChanges must be made through the merge queue"}`), false},
		{"required check 405 is not a conflict", respErr(http.StatusMethodNotAllowed, `{"message":"Required status check \"lint\" is expected."}`), false},
		{"conflict wording on a non-405 status", respErr(http.StatusConflict, `{"message":"Pull Request is not mergeable"}`), false},
		{"plain error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMergeConflictError(tc.err); got != tc.want {
				t.Fatalf("IsMergeConflictError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsForbiddenPATError pins #2685's classification constraint: only the
// exact fine-grained-PAT-checks-gap wording on a 403 may trigger the
// actions/runs fallback. GitHub returns 403 for unrelated reasons too (rate
// limiting, org SSO enforcement), and those must surface as ordinary errors.
func TestIsForbiddenPATError(t *testing.T) {
	respErr := func(status int, body string) error {
		return &providerResponseError{
			method:     http.MethodGet,
			endpoint:   "https://api.github.com/repos/o/r/commits/abc/check-runs",
			statusCode: status,
			body:       body,
		}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"fine-grained PAT checks gap", respErr(http.StatusForbidden, `{"message":"Resource not accessible by personal access token"}`), true},
		{"wrapped still detected", fmt.Errorf("check-runs: %w", respErr(http.StatusForbidden, `{"message":"Resource not accessible by personal access token"}`)), true},
		{"rate limited 403 is not this", respErr(http.StatusForbidden, `{"message":"API rate limit exceeded"}`), false},
		{"org SSO 403 is not this", respErr(http.StatusForbidden, `{"message":"Resource protected by organization SAML enforcement"}`), false},
		{"same wording on a non-403 status", respErr(http.StatusUnauthorized, `{"message":"Resource not accessible by personal access token"}`), false},
		{"plain error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsForbiddenPATError(tc.err); got != tc.want {
				t.Fatalf("IsForbiddenPATError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsRequiredStatusCheckPendingError(t *testing.T) {
	respErr := func(status int, body string) error {
		return &providerResponseError{
			method:     http.MethodPut,
			endpoint:   "https://api.github.com/repos/o/r/pulls/9/merge",
			statusCode: status,
			body:       body,
		}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"github required check 405", respErr(http.StatusMethodNotAllowed, `{"message":"Repository rule violations found\n\nRequired status check \"make ci\" is expected.\n\n"}`), true},
		{"message whitespace is normalized", respErr(http.StatusMethodNotAllowed, `{"message":"Required\nstatus check \"lint\" is expected."}`), true},
		{"wrapped still detected", fmt.Errorf("merge pull request: %w", respErr(http.StatusMethodNotAllowed, `{"message":"Required status check \"lint\" is expected."}`)), true},
		{"merge queue 405", respErr(http.StatusMethodNotAllowed, `{"message":"Repository rule violations found\n\nChanges must be made through the merge queue"}`), false},
		{"wording outside message", respErr(http.StatusMethodNotAllowed, `{"message":"Repository rule violations found","detail":"Required status check \"lint\" is expected."}`), false},
		{"required check on non-405 status", respErr(http.StatusConflict, `{"message":"Required status check \"lint\" is expected."}`), false},
		{"malformed response", respErr(http.StatusMethodNotAllowed, `Required status check "lint" is expected.`), false},
		{"plain error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRequiredStatusCheckPendingError(tc.err); got != tc.want {
				t.Fatalf("IsRequiredStatusCheckPendingError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsIdempotentHTTPMethod(t *testing.T) {
	for method, want := range map[string]bool{
		http.MethodGet:    true,
		http.MethodHead:   true,
		http.MethodPut:    true,
		http.MethodDelete: true,
		http.MethodPost:   false,
		http.MethodPatch:  false,
	} {
		if got := isIdempotentHTTPMethod(method); got != want {
			t.Errorf("isIdempotentHTTPMethod(%s) = %v, want %v", method, got, want)
		}
	}
}
