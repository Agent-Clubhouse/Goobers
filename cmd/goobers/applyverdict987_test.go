package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestDuplicateOfEarlierPR is #987: a later PR that references the same issue
// as an earlier open goober PR is a true duplicate (the #966/#969 case); the
// earlier one is not, and a PR referencing a different issue is not.
func TestDuplicateOfEarlierPR(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)

	// #966 and #969 both implement issue #774; #970 implements a different one.
	server.addIssue(966, "impl a")
	server.addOpenPR(966, "goobers/implementation/a", "main", "h966", "b", false, nil, nil)
	server.setPRBody(966, "## Summary\n\nImplements #774: convert logs.")
	server.addIssue(969, "impl b")
	server.addOpenPR(969, "goobers/implementation/b", "main", "h969", "b", false, nil, nil)
	server.setPRBody(969, "## Summary\n\nImplements #774: convert logs (again).")
	server.addIssue(970, "impl c")
	server.addOpenPR(970, "goobers/implementation/c", "main", "h970", "b", false, nil, nil)
	server.setPRBody(970, "## Summary\n\nImplements #775: something else.")

	provider := server.newGitHubProvider("token")
	ctx := context.Background()

	t.Run("the later duplicate is closable", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 969, Body: "Implements #774", State: "open"}
		reason, dup := duplicateOfEarlierPR(ctx, provider, repo, pr)
		if !dup {
			t.Fatalf("dup = false, want true; reason = %q", reason)
		}
	})
	t.Run("the earlier PR is not a duplicate of anything", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 966, Body: "Implements #774", State: "open"}
		if _, dup := duplicateOfEarlierPR(ctx, provider, repo, pr); dup {
			t.Fatal("the earliest PR for an issue must not be treated as a duplicate")
		}
	})
	t.Run("a PR for a different issue is not a duplicate", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 970, Body: "Implements #775", State: "open"}
		if _, dup := duplicateOfEarlierPR(ctx, provider, repo, pr); dup {
			t.Fatal("a PR referencing a different issue must not be a duplicate")
		}
	})
	t.Run("a PR referencing no issue is not a duplicate", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 999, Body: "no issue reference here", State: "open"}
		if _, dup := duplicateOfEarlierPR(ctx, provider, repo, pr); dup {
			t.Fatal("a PR with no issue reference must not be a duplicate")
		}
	})
}
