package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/fieldpredicate"
)

// titlePredicateCases is the #5810 cross-provider matrix shared by backlog
// work items and pull requests. Titles are fixed by titleFixtures; want lists
// the matching titles in fixture order.
var titlePredicateCases = []struct {
	name       string
	expression string
	want       []string
}{
	{name: "omitted", expression: "", want: []string{"[UI] Fix widget crash", "Release: widget v2", "fix gadget", ""}},
	{name: "exact", expression: `fields["title"] == "fix gadget"`, want: []string{"fix gadget"}},
	{name: "substring is case-sensitive", expression: `fields["title"].contains("Widget")`, want: nil},
	{name: "substring", expression: `fields["title"].contains("widget")`, want: []string{"[UI] Fix widget crash", "Release: widget v2"}},
	{name: "substring ignoring case", expression: `fields["title"].containsIgnoreCase("FIX")`, want: []string{"[UI] Fix widget crash", "fix gadget"}},
	{name: "prefix", expression: `fields["title"].startsWith("Release:")`, want: []string{"Release: widget v2"}},
	{name: "prefix is case-sensitive", expression: `fields["title"].startsWith("[ui]")`, want: nil},
	{name: "prefix ignoring case", expression: `fields["title"].startsWithIgnoreCase("[ui]")`, want: []string{"[UI] Fix widget crash"}},
	{name: "empty title", expression: `fields["title"] == ""`, want: []string{""}},
}

var titleFixtures = []string{"[UI] Fix widget crash", "Release: widget v2", "fix gadget", ""}

func TestWorkItemFieldsProjectNormalizedTitle(t *testing.T) {
	projections := map[string]func(title string) fieldpredicate.Fields{
		"github": func(title string) fieldpredicate.Fields {
			return githubIssueFields(githubIssue{Number: 1, Title: title, State: "open"})
		},
		"gitea": func(title string) fieldpredicate.Fields {
			return giteaIssueFields(giteaIssue{Number: 1, Title: title, State: "open"})
		},
		"ado": func(title string) fieldpredicate.Fields {
			return adoWorkItemFields(adoWorkItem{ID: 1, Fields: map[string]interface{}{"System.Title": title}})
		},
	}
	for provider, project := range projections {
		for _, tt := range titlePredicateCases {
			t.Run(provider+"/"+tt.name, func(t *testing.T) {
				predicate, err := fieldpredicate.Compile(tt.expression)
				if err != nil {
					t.Fatalf("Compile: %v", err)
				}
				var got []string
				for _, title := range titleFixtures {
					matched, err := predicate.Matches(project(title))
					if err != nil {
						t.Fatalf("Matches(%q): %v", title, err)
					}
					if matched {
						got = append(got, title)
					}
				}
				if !slices.Equal(got, tt.want) {
					t.Fatalf("matched %q, want %q", got, tt.want)
				}
			})
		}
	}

	ado := adoWorkItemFields(adoWorkItem{ID: 1, Fields: map[string]interface{}{"System.Title": "Native"}})
	if ado["System.Title"] != "Native" || ado["title"] != "Native" {
		t.Fatalf("ADO fields = %v, want native System.Title preserved alongside title", ado)
	}
	predicate, err := fieldpredicate.Compile(`fields["title"].contains("x")`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, err := predicate.Matches(adoWorkItemFields(adoWorkItem{ID: 1, Fields: map[string]interface{}{}})); err == nil ||
		!strings.Contains(err.Error(), `field "title" is unavailable`) {
		t.Fatalf("missing ADO title error = %v, want unavailable-field error", err)
	}
}

func TestListPullRequestsTitlePredicate(t *testing.T) {
	listers := map[string]func(t *testing.T) (func(context.Context, ListPullRequestsRequest) ([]PullRequestSummary, error), RepositoryRef){
		"github": func(t *testing.T) (func(context.Context, ListPullRequestsRequest) ([]PullRequestSummary, error), RepositoryRef) {
			server := titlePullServer(t, "/repos/acme/app/pulls", func(i int, title string) map[string]interface{} {
				return restTitlePull(i, title)
			})
			provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
			return provider.ListPullRequests, RepositoryRef{Owner: "acme", Name: "app"}
		},
		"gitea": func(t *testing.T) (func(context.Context, ListPullRequestsRequest) ([]PullRequestSummary, error), RepositoryRef) {
			server := titlePullServer(t, "/api/v1/repos/acme/app/pulls", func(i int, title string) map[string]interface{} {
				return restTitlePull(i, title)
			})
			provider := NewGiteaProvider(server.URL, "token")
			return provider.ListPullRequests, RepositoryRef{Owner: "acme", Name: "app"}
		},
		"ado": func(t *testing.T) (func(context.Context, ListPullRequestsRequest) ([]PullRequestSummary, error), RepositoryRef) {
			server := titlePullServer(t, "/org/project/_apis/git/repositories/repo/pullrequests", func(i int, title string) map[string]interface{} {
				return map[string]interface{}{
					"pullRequestId": i + 1, "title": title, "status": "active",
					"sourceRefName": "refs/heads/goobers/run", "targetRefName": "refs/heads/main",
				}
			})
			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			return provider.ListPullRequests, RepositoryRef{Name: "repo", Project: "project"}
		},
	}
	for provider, newLister := range listers {
		list, repo := newLister(t)
		for _, tt := range titlePredicateCases {
			t.Run(provider+"/"+tt.name, func(t *testing.T) {
				predicate, err := fieldpredicate.CompileTitlePredicate(tt.expression)
				if err != nil {
					t.Fatalf("CompileTitlePredicate: %v", err)
				}
				prs, err := list(context.Background(), ListPullRequestsRequest{
					Repository: repo, Base: "main", HeadPrefix: "goobers/", SkipCheckState: true,
					TitlePredicate: predicate,
				})
				if err != nil {
					t.Fatalf("ListPullRequests: %v", err)
				}
				var got []string
				for _, pr := range prs {
					got = append(got, pr.Title)
				}
				if !slices.Equal(got, tt.want) {
					t.Fatalf("listed titles %q, want %q", got, tt.want)
				}
			})
		}
	}
}

func TestCompileTitlePredicateRejectsNonTitleFields(t *testing.T) {
	for _, expression := range []string{`fields["state"] == "open"`, `fields["title"].contains("x") && fields["number"] > 1`} {
		if _, err := fieldpredicate.CompileTitlePredicate(expression); err == nil ||
			!strings.Contains(err.Error(), `may only reference fields["title"]`) {
			t.Fatalf("CompileTitlePredicate(%q) error = %v, want title-only error", expression, err)
		}
	}
}

func restTitlePull(i int, title string) map[string]interface{} {
	return map[string]interface{}{
		"number": i + 1, "title": title, "state": "open", "updated_at": "2026-07-15T00:00:00Z",
		"head": map[string]interface{}{"ref": "goobers/run", "sha": "head"},
		"base": map[string]interface{}{"ref": "main", "sha": "base"},
	}
}

func titlePullServer(t *testing.T, path string, pull func(int, string) map[string]interface{}) *httptest.Server {
	t.Helper()
	pulls := make([]map[string]interface{}, 0, len(titleFixtures))
	for i, title := range titleFixtures {
		pulls = append(pulls, pull(i, title))
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(path, "_apis") {
			writeJSON(t, w, map[string]interface{}{"value": pulls})
			return
		}
		writeJSON(t, w, pulls)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}
