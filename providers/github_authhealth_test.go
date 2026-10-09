package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubVerifyRepositoryReadAccess(t *testing.T) {
	cases := []struct {
		name      string
		repo      func(http.ResponseWriter)
		issues    int
		wantIssue bool
		check     func(error) bool
	}{
		{name: "valid", repo: jsonBody(`{"permissions":{"pull":true}}`), issues: http.StatusOK, wantIssue: true,
			check: func(err error) bool { return err == nil }},
		{name: "fine-grained token without permissions block", repo: jsonBody(`{}`), issues: http.StatusOK, wantIssue: true,
			check: func(err error) bool { return err == nil }},
		{name: "revoked", repo: statusOnly(http.StatusUnauthorized),
			check: IsUnauthorizedError},
		{name: "forbidden", repo: statusOnly(http.StatusForbidden),
			check: func(err error) bool { return IsAuthenticationError(err) && !IsUnauthorizedError(err) }},
		{name: "repository hidden", repo: statusOnly(http.StatusNotFound),
			check: IsNotFoundError},
		{name: "pull denied", repo: jsonBody(`{"permissions":{"pull":false}}`),
			check: func(err error) bool { return errors.Is(err, ErrInsufficientRepositoryPermission) }},
		{name: "issues unreadable", repo: jsonBody(`{"permissions":{"pull":true}}`), issues: http.StatusForbidden, wantIssue: true,
			check: IsAuthenticationError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sawIssues := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("health check issued mutating %s %s", r.Method, r.URL.Path)
				}
				switch r.URL.Path {
				case "/repos/acme/web":
					tc.repo(w)
				case "/repos/acme/web/issues":
					sawIssues = true
					if r.URL.Query().Get("per_page") != "1" {
						t.Errorf("issues probe query = %q", r.URL.RawQuery)
					}
					w.WriteHeader(tc.issues)
					_, _ = w.Write([]byte(`[]`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL; p.maxRetries = 0 })
			err := provider.VerifyRepositoryReadAccess(context.Background(), RepositoryRef{Owner: "acme", Name: "web"})
			if !tc.check(err) {
				t.Fatalf("VerifyRepositoryReadAccess error = %v", err)
			}
			if sawIssues != tc.wantIssue {
				t.Fatalf("issues probed = %v, want %v", sawIssues, tc.wantIssue)
			}
		})
	}
}

func jsonBody(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func statusOnly(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"message":"denied"}`))
	}
}
