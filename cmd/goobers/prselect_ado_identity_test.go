package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/providers"
)

func TestADOSelectionIdentityFiltersIgnoreCase(t *testing.T) {
	for _, trigger := range []string{"", "github-webhook:pull_request#42"} {
		for _, tc := range []struct {
			name, author, reviewer, title string
			want                          int
		}{
			{"both", "author@example.com", "reviewer@example.com", "", 1},
			{"author mismatch", "other@example.com", "reviewer@example.com", "", 0},
			{"reviewer mismatch", "author@example.com", "other@example.com", "", 0},
			{"title mismatch", "author@example.com", "reviewer@example.com", `fields["title"] == "other"`, 0},
		} {
			t.Run(trigger+"/"+tc.name, func(t *testing.T) {
				pr := providers.PullRequestSummary{Number: 42, Title: "change", Author: "Author@Example.COM", RequestedReviewers: []string{"Reviewer@Example.COM"}, Head: "topic", Base: "main"}
				fake := &fakeADOSelectProvider{open: []providers.PullRequestSummary{pr}, polls: map[int]providers.PullRequestPollResult{42: {Number: 42, Title: pr.Title, Author: pr.Author, RequestedReviewers: pr.RequestedReviewers, HeadBranch: pr.Head, BaseBranch: pr.Base, CheckState: providers.CheckStatePassing, State: "open"}}}
				filters := providers.ListPullRequestsRequest{Author: tc.author, RequestedReviewer: tc.reviewer}
				var err error
				filters.TitlePredicate, err = fieldpredicate.CompileTitlePredicate(tc.title)
				if err != nil {
					t.Fatal(err)
				}
				got, _, err := pullRequestsForSelectionADO(context.Background(), fake, providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}, "main", nil, authorScopeAny, filters, trigger, prSelectPartialSnapshot, "")
				if err != nil || len(got) != tc.want {
					t.Fatalf("got=%+v err=%v", got, err)
				}
				if len(got) > 0 && (got[0].Author != pr.Author || got[0].RequestedReviewers[0] != pr.RequestedReviewers[0]) {
					t.Fatalf("spelling changed: %+v", got[0])
				}
			})
		}
	}
}

func TestSelectionDefaultIdentityMatchingRemainsCaseSensitive(t *testing.T) {
	got, _, err := pullRequestsForSelectionCommon(context.Background(), []providers.PullRequestSummary{{Number: 42, Author: "Author@Example.COM"}}, prSelectSourceRequest{authorScope: authorScopeAny, identityFilters: providers.ListPullRequestsRequest{Author: "author@example.com"}}, selectionResolver{resolve: func(context.Context, *providers.PullRequestSummary) error {
		t.Fatal("mismatch reached resolver")
		return nil
	}})
	if err != nil || len(got) != 0 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
