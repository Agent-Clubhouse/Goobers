package providers

import (
	"reflect"
	"testing"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

func TestGitHubPullRequestSummaryProjection(t *testing.T) {
	mergedAt := time.Date(2026, 10, 1, 11, 30, 0, 0, time.UTC)
	updatedAt := mergedAt.Add(time.Hour)
	pr := githubPullRequestDetail{
		Number:             42,
		State:              "closed",
		MergedAt:           &mergedAt,
		MergeCommitSHA:     "merge-sha",
		Draft:              true,
		Body:               "Fixes #41",
		HTMLURL:            "https://github.test/acme/app/pull/42",
		User:               githubUser{Login: "author"},
		Assignees:          []githubUser{{Login: "assignee-1"}, {Login: "assignee-2"}},
		RequestedReviewers: []githubUser{{Login: "reviewer-1"}, {Login: "reviewer-2"}},
		Labels:             []githubLabel{{Name: "second"}, {Name: "first"}},
		UpdatedAt:          updatedAt,
	}
	pr.Head.Ref = "feature/shared-projection"
	pr.Head.SHA = "head-sha"
	pr.Base.Ref = "main"
	pr.Base.SHA = "base-sha"

	got := summarizePullRequest(pr, CheckStatePassing)
	want := PullRequestSummary{
		ID:                 "42",
		Number:             42,
		URL:                "https://github.test/acme/app/pull/42",
		Author:             "author",
		Assignees:          []string{"assignee-1", "assignee-2"},
		RequestedReviewers: []string{"reviewer-1", "reviewer-2"},
		State:              "closed",
		Merged:             true,
		Head:               "feature/shared-projection",
		Base:               "main",
		HeadSHA:            "head-sha",
		BaseSHA:            "base-sha",
		MergeSHA:           "merge-sha",
		Draft:              true,
		Labels:             []string{"second", "first"},
		CheckState:         CheckStatePassing,
		UpdatedAt:          updatedAt,
		Body:               "Fixes #41",
		Integrity:          apiintegrity.Unapproved,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summarizePullRequest() = %+v, want %+v", got, want)
	}
}

func TestGiteaPullRequestSummaryProjection(t *testing.T) {
	mergedAt := time.Date(2026, 10, 2, 9, 15, 0, 0, time.UTC)
	updatedAt := mergedAt.Add(2 * time.Hour)
	pr := giteaPull{
		Number:         17,
		Title:          "  wip: shared projection",
		Body:           "Closes #16",
		State:          "closed",
		HTMLURL:        "https://gitea.test/acme/app/pulls/17",
		MergedAt:       &mergedAt,
		MergeCommitSHA: "gitea-merge-sha",
		Labels:         []giteaLabel{{Name: "beta"}, {Name: "alpha"}},
		UpdatedAt:      updatedAt,
		Head:           giteaPRBranch{Ref: "feature/gitea-projection", SHA: "gitea-head-sha"},
		Base:           giteaPRBranch{Ref: "trunk", SHA: "gitea-base-sha"},
	}

	got := summarizeGiteaPull(pr, CheckStateFailing)
	want := PullRequestSummary{
		ID:         "17",
		Number:     17,
		URL:        "https://gitea.test/acme/app/pulls/17",
		State:      "closed",
		Merged:     true,
		Head:       "feature/gitea-projection",
		Base:       "trunk",
		HeadSHA:    "gitea-head-sha",
		BaseSHA:    "gitea-base-sha",
		MergeSHA:   "gitea-merge-sha",
		Draft:      true,
		Labels:     []string{"beta", "alpha"},
		CheckState: CheckStateFailing,
		UpdatedAt:  updatedAt,
		Body:       "Closes #16",
		Integrity:  apiintegrity.Unapproved,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summarizeGiteaPull() = %+v, want %+v", got, want)
	}
}
