package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

const githubPRProjectionFixture = `{
	"id": 9001,
	"number": 42,
	"title": "Fix projection",
	"body": "Fixes #17",
	"html_url": "https://github.test/acme/app/pull/42",
	"state": "closed",
	"merged": false,
	"merged_at": "2026-09-01T12:00:00Z",
	"merge_commit_sha": "merge-sha",
	"mergeable": false,
	"mergeable_state": "dirty",
	"draft": true,
	"user": {"login": "author"},
	"assignees": [{"login": "zoe"}, {"login": "alice"}],
	"requested_reviewers": [{"login": "reviewer-z"}, {"login": "reviewer-a"}],
	"labels": [{"name": "z-last"}, {"name": "a-first"}, {"name": "z-last"}],
	"updated_at": "2026-09-02T13:00:00Z",
	"head": {
		"ref": "goobers/work",
		"sha": "head-sha",
		"repo": {"name": "fork", "html_url": "https://github.test/forker/fork", "owner": {"login": "forker"}}
	},
	"base": {"ref": "release", "sha": "base-sha"}
}`

const giteaPRProjectionFixture = `{
	"id": 9002,
	"number": 43,
	"title": "  wIp: Fix projection",
	"body": "Closes #18",
	"html_url": "https://gitea.test/acme/app/pulls/43",
	"state": "closed",
	"merged": false,
	"merged_at": "2026-09-01T12:00:00Z",
	"merge_commit_sha": "merge-sha",
	"mergeable": false,
	"mergeable_state": "dirty",
	"draft": false,
	"user": {"login": "ignored-author"},
	"assignees": [{"login": "ignored-assignee"}],
	"requested_reviewers": [{"login": "ignored-reviewer"}],
	"labels": [{"name": "z-last"}, {"name": "a-first"}, {"name": "z-last"}],
	"updated_at": "2026-09-02T13:00:00Z",
	"head": {
		"ref": "goobers/work",
		"sha": "head-sha",
		"repo": {"name": "fork", "html_url": "https://gitea.test/forker/fork", "owner": {"login": "forker"}}
	},
	"base": {"ref": "release", "sha": "base-sha"}
}`

func TestGitHubPullRequestSummaryMapper(t *testing.T) {
	var pr githubPullRequestDetail
	if err := json.Unmarshal([]byte(githubPRProjectionFixture), &pr); err != nil {
		t.Fatal(err)
	}
	want := PullRequestSummary{
		ID: "42", Number: 42, URL: "https://github.test/acme/app/pull/42",
		Author: "author", Assignees: []string{"zoe", "alice"},
		RequestedReviewers: []string{"reviewer-z", "reviewer-a"},
		State:              "closed", Merged: true, Draft: true,
		Head: "goobers/work", Base: "release", HeadSHA: "head-sha", BaseSHA: "base-sha",
		MergeSHA: "merge-sha", Labels: []string{"z-last", "a-first", "z-last"},
		CheckState: CheckStateFailing, UpdatedAt: time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC),
		Body: "Fixes #17", Integrity: apiintegrity.Unapproved,
	}
	if got := summarizePullRequest(pr, CheckStateFailing); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}

	t.Run("merged flag without timestamp", func(t *testing.T) {
		pr := pr
		pr.Merged, pr.MergedAt = true, nil
		if got := summarizePullRequest(pr, CheckStateFailing); !reflect.DeepEqual(got, want) {
			t.Fatalf("summary = %#v, want %#v", got, want)
		}
	})
	t.Run("open non-draft ignores WIP title", func(t *testing.T) {
		pr, want := pr, want
		pr.State, pr.Merged, pr.MergedAt, pr.Draft, pr.Title = "open", false, nil, false, "WIP: Fix projection"
		want.State, want.Merged, want.Draft, want.CheckState = "open", false, false, ""
		if got := summarizePullRequest(pr, ""); !reflect.DeepEqual(got, want) {
			t.Fatalf("summary = %#v, want %#v", got, want)
		}
	})
	t.Run("missing fields preserve empty slices", func(t *testing.T) {
		want := PullRequestSummary{
			ID: "0", Labels: []string{}, Assignees: []string{}, RequestedReviewers: []string{},
			Integrity: apiintegrity.Unapproved,
		}
		if got := summarizePullRequest(githubPullRequestDetail{}, ""); !reflect.DeepEqual(got, want) {
			t.Fatalf("summary = %#v, want %#v", got, want)
		}
	})
}

func TestGiteaPullRequestSummaryMapper(t *testing.T) {
	var pr giteaPull
	if err := json.Unmarshal([]byte(giteaPRProjectionFixture), &pr); err != nil {
		t.Fatal(err)
	}
	want := PullRequestSummary{
		ID: "43", Number: 43, URL: "https://gitea.test/acme/app/pulls/43",
		State: "closed", Merged: true, Draft: true,
		Head: "goobers/work", Base: "release", HeadSHA: "head-sha", BaseSHA: "base-sha",
		MergeSHA: "merge-sha", Labels: []string{"z-last", "a-first", "z-last"},
		CheckState: CheckStatePending, UpdatedAt: time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC),
		Body: "Closes #18", Integrity: apiintegrity.Unapproved,
	}
	if got := summarizeGiteaPull(pr, CheckStatePending); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}

	t.Run("merged flag without timestamp", func(t *testing.T) {
		pr := pr
		pr.Merged, pr.MergedAt = true, nil
		if got := summarizeGiteaPull(pr, CheckStatePending); !reflect.DeepEqual(got, want) {
			t.Fatalf("summary = %#v, want %#v", got, want)
		}
	})
	t.Run("open non-draft", func(t *testing.T) {
		pr, want := pr, want
		pr.State, pr.Merged, pr.MergedAt, pr.Title = "open", false, nil, "Fix projection"
		want.State, want.Merged, want.Draft, want.CheckState = "open", false, false, ""
		if got := summarizeGiteaPull(pr, ""); !reflect.DeepEqual(got, want) {
			t.Fatalf("summary = %#v, want %#v", got, want)
		}
	})
	t.Run("missing fields preserve absent identities", func(t *testing.T) {
		want := PullRequestSummary{ID: "0", Labels: []string{}, Integrity: apiintegrity.Unapproved}
		if got := summarizeGiteaPull(giteaPull{}, ""); !reflect.DeepEqual(got, want) {
			t.Fatalf("summary = %#v, want %#v", got, want)
		}
	})
}

func TestGiteaPullRequestSummaryWIPDraft(t *testing.T) {
	for _, tc := range []struct {
		title string
		draft bool
	}{
		{"WIP: Fix", true},
		{" \twiP: Fix", true},
		{"WIP:", true},
		{"WIP Fix", false},
		{"Fix WIP: handling", false},
		{"Fix", false},
		{"", false},
	} {
		t.Run(tc.title, func(t *testing.T) {
			if got := summarizeGiteaPull(giteaPull{Title: tc.title}, ""); got.Draft != tc.draft {
				t.Fatalf("Draft = %v, want %v for title %q", got.Draft, tc.draft, tc.title)
			}
		})
	}
}

func TestGitHubPullRequestPollProjection(t *testing.T) {
	var pr githubPullRequestDetail
	if err := json.Unmarshal([]byte(githubPRProjectionFixture), &pr); err != nil {
		t.Fatal(err)
	}
	mergedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mergeable := false
	want := PullRequestPollResult{
		Number: 42, Title: "Fix projection", URL: "https://github.test/acme/app/pull/42",
		Author: "author", Assignees: []string{"zoe", "alice"},
		RequestedReviewers: []string{"reviewer-z", "reviewer-a"},
		State:              "closed", Merged: false, MergedAt: &mergedAt, Draft: true,
		Mergeable: &mergeable, MergeableState: "dirty",
		Labels:     []string{"z-last", "a-first", "z-last"},
		HeadBranch: "goobers/work", HeadSHA: "head-sha", BaseBranch: "release", BaseSHA: "base-sha",
		HeadRepository: &RepositoryRef{
			Provider: ProviderGitHub, Owner: "forker", Name: "fork", URL: "https://github.test/forker/fork",
		},
		Body: "Fixes #17", ReviewDecision: ReviewDecisionChangesRequested, RequestedChanges: 1,
		CheckState: CheckStateFailing,
		Checks: []CheckDetail{
			{Name: "ci", State: CheckStateFailing, Conclusion: "failure", URL: "https://ci.test", Summary: "failed"},
		},
		CommentsSince: []PullRequestComment{
			{ID: 7, Author: "commenter", Body: "Please fix", URL: "https://comments.test/7", CreatedAt: mergedAt, Integrity: apiintegrity.Unapproved},
		},
		Integrity: apiintegrity.Unapproved,
	}
	got := pullPollResultFromProjection(githubPullRequestProjection(pr), want.ReviewDecision, want.RequestedChanges, want.CheckState, want.Checks, want.CommentsSince)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("poll projection = %#v, want %#v", got, want)
	}
	assertRESTPullRequestPollProjection(t, ProviderGitHub, githubPRProjectionFixture, want)

	t.Run("missing fields preserve unknown mergeability", func(t *testing.T) {
		got := pullPollResultFromProjection(githubPullRequestProjection(githubPullRequestDetail{}), "", 0, "", nil, nil)
		want := PullRequestPollResult{
			Labels: []string{}, Assignees: []string{}, RequestedReviewers: []string{},
			Integrity: apiintegrity.Unapproved,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("poll projection = %#v, want %#v", got, want)
		}
	})
}

func TestGiteaPullRequestPollProjection(t *testing.T) {
	var pr giteaPull
	if err := json.Unmarshal([]byte(giteaPRProjectionFixture), &pr); err != nil {
		t.Fatal(err)
	}
	mergedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mergeable := false
	want := PullRequestPollResult{
		Number: 43, Title: "  wIp: Fix projection", URL: "https://gitea.test/acme/app/pulls/43",
		State: "closed", Merged: false, MergedAt: &mergedAt, Draft: true,
		Mergeable: &mergeable, Labels: []string{"z-last", "a-first", "z-last"},
		HeadBranch: "goobers/work", HeadSHA: "head-sha", BaseBranch: "release", BaseSHA: "base-sha",
		HeadRepository: &RepositoryRef{
			Provider: ProviderGitea, Owner: "forker", Name: "fork", URL: "https://gitea.test/forker/fork",
		},
		Body: "Closes #18", ReviewDecision: ReviewDecisionChangesRequested, RequestedChanges: 1,
		CheckState: CheckStateFailing,
		Checks: []CheckDetail{
			{Name: "ci", State: CheckStateFailing, Conclusion: "failure", URL: "https://ci.test", Summary: "failed"},
		},
		CommentsSince: []PullRequestComment{
			{ID: 7, Author: "commenter", Body: "Please fix", URL: "https://comments.test/7", CreatedAt: mergedAt, Integrity: apiintegrity.Unapproved},
		},
		Integrity: apiintegrity.Unapproved,
	}
	got := pullPollResultFromProjection(giteaPullProjection(pr), want.ReviewDecision, want.RequestedChanges, want.CheckState, want.Checks, want.CommentsSince)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("poll projection = %#v, want %#v", got, want)
	}
	assertRESTPullRequestPollProjection(t, ProviderGitea, giteaPRProjectionFixture, want)

	t.Run("missing fields preserve absent identities", func(t *testing.T) {
		got := pullPollResultFromProjection(giteaPullProjection(giteaPull{}), "", 0, "", nil, nil)
		want := PullRequestPollResult{Labels: []string{}, Integrity: apiintegrity.Unapproved}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("poll projection = %#v, want %#v", got, want)
		}
	})
}

func assertRESTPullRequestPollProjection(t *testing.T, kind ProviderKind, fixture string, want PullRequestPollResult) {
	t.Helper()
	prefix, reviewState := "", "CHANGES_REQUESTED"
	if kind == ProviderGitea {
		prefix, reviewState = "/api/v1", "REQUEST_CHANGES"
	}
	mux := http.NewServeMux()
	pullID := strconv.Itoa(want.Number)
	mux.HandleFunc(prefix+"/repos/acme/app/pulls/"+pullID, func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, json.RawMessage(fixture))
	})
	mux.HandleFunc(prefix+"/repos/acme/app/pulls/"+pullID+"/reviews", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"state": reviewState, "user": map[string]string{"login": "reviewer"}},
		})
	})
	mux.HandleFunc(prefix+"/repos/acme/app/commits/head-sha/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"state": "failure",
			"statuses": []map[string]string{
				{"context": "ci", "state": "failure", "status": "failure", "target_url": "https://ci.test", "description": "failed"},
			},
		})
	})
	mux.HandleFunc(prefix+"/repos/acme/app/commits/head-sha/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"check_runs": []interface{}{}})
	})
	mux.HandleFunc(prefix+"/repos/acme/app/issues/"+pullID+"/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"id": 7, "user": map[string]string{"login": "commenter"}, "body": "Please fix", "html_url": "https://comments.test/7", "created_at": "2026-09-01T12:00:00Z"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	var provider RepoProvider = NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	if kind == ProviderGitea {
		provider = NewGiteaProvider(server.URL, "token")
	}
	got, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: pullID,
	})
	if err != nil {
		t.Fatalf("PollPullRequest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PollPullRequest = %#v, want %#v", got, want)
	}
}
