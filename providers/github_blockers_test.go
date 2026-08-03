package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGitHubHasOpenWorkItemBlocker pins backlog.blockers for GitHub — this
// capability had zero test coverage anywhere in the repo before CONF-4
// (#2077). GitHub's native "blocked_by" dependencies API is the source of
// truth: a blocker only counts if it's a real issue (not a pull request)
// and still open.
func TestGitHubHasOpenWorkItemBlocker(t *testing.T) {
	tests := []struct {
		name   string
		issues []map[string]interface{}
		want   bool
	}{
		{name: "no blockers", issues: nil, want: false},
		{
			name: "one open blocker",
			issues: []map[string]interface{}{
				{"id": 1, "number": 1, "title": "blocker", "state": "open"},
			},
			want: true,
		},
		{
			name: "only closed blockers",
			issues: []map[string]interface{}{
				{"id": 1, "number": 1, "title": "old blocker", "state": "closed"},
			},
			want: false,
		},
		{
			name: "a blocking pull request is not a blocker",
			issues: []map[string]interface{}{
				{"id": 1, "number": 1, "title": "pr", "state": "open", "pull_request": map[string]string{"url": "pr-url"}},
			},
			want: false,
		},
		{
			name: "closed blocker alongside an open one still reports true",
			issues: []map[string]interface{}{
				{"id": 1, "number": 1, "title": "closed", "state": "closed"},
				{"id": 2, "number": 2, "title": "open", "state": "open"},
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Path; got != "/repos/acme/app/issues/7/dependencies/blocked_by" {
					t.Fatalf("path = %q", got)
				}
				writeJSON(t, w, tc.issues)
			}))
			defer server.Close()
			provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })

			got, err := provider.HasOpenWorkItemBlocker(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
			if err != nil {
				t.Fatalf("HasOpenWorkItemBlocker: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasOpenWorkItemBlocker = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGitHubHasOpenWorkItemBlockerStopsAtFirstOpenBlockerAcrossPages proves
// the pagination path: an open blocker on a later page must still be
// found, and the scan must stop as soon as one is found rather than
// fetching every remaining page.
func TestGitHubHasOpenWorkItemBlockerStopsAtFirstOpenBlockerAcrossPages(t *testing.T) {
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues/7/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("page") == "2" {
			writeJSON(t, w, []map[string]interface{}{
				{"id": 2, "number": 2, "title": "open blocker on page 2", "state": "open"},
				{"id": 3, "number": 3, "title": "never reached", "state": "open"},
			})
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "number": 1, "title": "closed blocker", "state": "closed"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })

	got, err := provider.HasOpenWorkItemBlocker(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err != nil {
		t.Fatalf("HasOpenWorkItemBlocker: %v", err)
	}
	if !got {
		t.Fatalf("HasOpenWorkItemBlocker = false, want true (open blocker on page 2)")
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want exactly 2 (stop as soon as the open blocker is found)", requests)
	}
}

// TestGitHubHasOpenWorkItemBlockerRequiresOwnerRepoAndID pins the
// error-path contract: a missing repo owner/name or a blank id must fail
// closed rather than silently reporting "no blocker".
func TestGitHubHasOpenWorkItemBlockerRequiresOwnerRepoAndID(t *testing.T) {
	provider := NewGitHubProvider("token")

	if _, err := provider.HasOpenWorkItemBlocker(context.Background(), RepositoryRef{}, "7"); err == nil {
		t.Fatal("HasOpenWorkItemBlocker with no owner/repo: want error, got nil")
	}
	if _, err := provider.HasOpenWorkItemBlocker(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, ""); err == nil {
		t.Fatal("HasOpenWorkItemBlocker with blank id: want error, got nil")
	}
}
