package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

func seedTitledPRSelectServer(t *testing.T) *fakeGitHubServer {
	t.Helper()
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for _, fixture := range []struct {
		number int
		title  string
		head   string
	}{
		{5810, "[UI] Fix widget crash", "feature/ui-widget"},
		{5811, "Release: widget v2", "feature/release-widget"},
	} {
		server.addIssue(fixture.number, fixture.title)
		server.addOpenPR(fixture.number, fixture.head, "main", fmt.Sprintf("head%d", fixture.number), "main-base", false, nil, nil)
		server.mu.Lock()
		server.prs[fixture.number].title = fixture.title
		server.mu.Unlock()
	}
	return server
}

func TestPRSelectTitlePredicateFiltersCandidates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		predicate string
		want      string
	}{
		{"prefix ignoring case", `fields["title"].startsWithIgnoreCase("[ui]")`, "5810"},
		{"substring", `fields["title"].contains("Release")`, "5811"},
		{"omitted", "", "5810"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := seedTitledPRSelectServer(t)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "title-5810")
			routeMergeReviewTestRepo(t)
			t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
			t.Setenv(executor.InputEnvVar("titlePredicate"), tc.predicate)

			t.Chdir(t.TempDir())
			if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
				t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			data, err := os.ReadFile("selected-pr.json")
			if err != nil {
				t.Fatal(err)
			}
			var selected map[string]any
			if err := json.Unmarshal(data, &selected); err != nil {
				t.Fatal(err)
			}
			if got, _ := selected["number"].(string); got != tc.want {
				t.Fatalf("selected PR = %v, want #%s", selected, tc.want)
			}
		})
	}
}

func TestPRSelectRejectsInvalidTitlePredicate(t *testing.T) {
	root := initDemo(t)
	server := seedTitledPRSelectServer(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "title-5810")
	routeMergeReviewTestRepo(t)
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("titlePredicate"), `fields["author"] == "octocat"`)

	t.Chdir(t.TempDir())
	code, _, stderr := runArgs(t, "pr-select", root)
	if code != 1 || !strings.Contains(stderr, "invalid titlePredicate") {
		t.Fatalf("pr-select: code = %d, stderr = %q, want invalid titlePredicate failure", code, stderr)
	}
}
