package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFirstParentHistoryFollowsOnlyTheBranchItself covers #7071's base
// history read on both providers: the chain follows first parents from the
// tip to the stop commit, never a merged branch's commits, and is returned
// only when it reaches stop within the limit on the listed page.
func TestFirstParentHistoryFollowsOnlyTheBranchItself(t *testing.T) {
	page := []map[string]interface{}{
		{"sha": "tip", "parents": []map[string]string{{"sha": "mid"}, {"sha": "branch"}}},
		{"sha": "branch", "parents": []map[string]string{{"sha": "stop"}}},
		{"sha": "mid", "parents": []map[string]string{{"sha": "old"}}},
		{"sha": "old", "parents": []map[string]string{{"sha": "stop"}}},
		{"sha": "stop", "parents": []map[string]string{{"sha": "root"}}},
	}
	type historyReader interface {
		FirstParentHistory(ctx context.Context, repo RepositoryRef, tip, stop string, limit int) ([]string, error)
	}
	providers := []struct {
		name     string
		path     string
		pageSize string
		build    func(url string) historyReader
	}{
		{name: "github", path: "/repos/acme/app/commits", pageSize: "per_page", build: func(url string) historyReader {
			return NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = url })
		}},
		{name: "gitea", path: "/api/v1/repos/acme/app/commits", pageSize: "limit", build: func(url string) historyReader {
			return NewGiteaProvider(url, "token")
		}},
	}
	// stop is reached only through tip's merged branch, as for a PR stacked on
	// a branch since merged into the base: first parents run past it.
	offChain := []map[string]interface{}{
		{"sha": "tip", "parents": []map[string]string{{"sha": "mid"}, {"sha": "branch"}}},
		{"sha": "branch", "parents": []map[string]string{{"sha": "stop"}}},
		{"sha": "stop", "parents": []map[string]string{{"sha": "old"}}},
		{"sha": "mid", "parents": []map[string]string{{"sha": "old"}}},
		{"sha": "old", "parents": []map[string]string{{"sha": "root"}}},
		{"sha": "root"},
	}
	cases := []struct {
		name  string
		page  []map[string]interface{}
		limit int
		want  string
	}{
		{name: "whole chain", page: page, limit: 100, want: "tip,mid,old"},
		{name: "chain exactly at limit", page: page, limit: 3, want: "tip,mid,old"},
		{name: "chain over limit", page: page, limit: 2, want: ""},
		{name: "page ends before stop", page: page[:3], limit: 100, want: ""},
		{name: "stop off the first-parent chain", page: offChain, limit: 100, want: ""},
	}
	for _, provider := range providers {
		for _, tc := range cases {
			t.Run(provider.name+"/"+tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assertMethod(t, r, http.MethodGet)
					if r.URL.Path != provider.path || r.URL.Query().Get("sha") != "tip" || r.URL.Query().Get(provider.pageSize) != "100" {
						t.Errorf("request = %s, want %s listed from tip", r.URL, provider.path)
					}
					writeJSON(t, w, tc.page)
				}))
				defer server.Close()

				got, err := provider.build(server.URL).FirstParentHistory(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "tip", "stop", tc.limit)
				if err != nil {
					t.Fatalf("FirstParentHistory: %v", err)
				}
				if strings.Join(got, ",") != tc.want {
					t.Fatalf("history = %v, want %s", got, tc.want)
				}
			})
		}
	}
}
