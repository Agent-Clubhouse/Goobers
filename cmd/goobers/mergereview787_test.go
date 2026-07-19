package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestMergeReviewStatusCommentLifecycle(t *testing.T) {
	const prNumber = 787
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "sticky status")
	const humanComment = "Human note quoting <!-- goobers:merge-review-status -->; preserve this exactly."
	server.addComment(prNumber, humanComment)
	provider := server.newGitHubProvider("token")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	first := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "first cycle"})
	if err := reconcileMergeReviewStatusComment(context.Background(), provider, repo, prNumber, first); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	comments, ids := fakeIssueComments(t, server, prNumber)
	assertCanonicalMergeReviewStatus(t, comments, first, humanComment)
	firstID := markedCommentID(t, comments, ids)

	second := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "later cycle"})
	if err := reconcileMergeReviewStatusComment(context.Background(), provider, repo, prNumber, second); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	comments, ids = fakeIssueComments(t, server, prNumber)
	assertCanonicalMergeReviewStatus(t, comments, second, humanComment)
	if got := markedCommentID(t, comments, ids); got != firstID {
		t.Fatalf("status comment id = %d, want original id %d", got, firstID)
	}

	if err := provider.DeleteComment(context.Background(), repo, fmt.Sprint(firstID)); err != nil {
		t.Fatalf("delete marked comment: %v", err)
	}
	recreated := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictFail, Summary: "recreated cycle"})
	if err := reconcileMergeReviewStatusComment(context.Background(), provider, repo, prNumber, recreated); err != nil {
		t.Fatalf("reconcile after deletion: %v", err)
	}
	comments, ids = fakeIssueComments(t, server, prNumber)
	assertCanonicalMergeReviewStatus(t, comments, recreated, humanComment)
	if got := markedCommentID(t, comments, ids); got == firstID {
		t.Fatalf("recreated status comment retained deleted id %d", firstID)
	}
}

func TestConcurrentMergeReviewStatusUpdatesConverge(t *testing.T) {
	const prNumber = 788
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "concurrent sticky status")
	const humanComment = "<!-- unrelated -->\nHuman-authored automation note."
	server.addComment(prNumber, humanComment)
	provider := server.newGitHubProvider("token")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	const updates = 8
	errs := make(chan error, updates)
	var wg sync.WaitGroup
	for i := 0; i < updates; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := renderVerdictComment(apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Summary:  fmt.Sprintf("retry %d", i),
			})
			errs <- reconcileMergeReviewStatusComment(context.Background(), provider, repo, prNumber, body)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reconcile: %v", err)
		}
	}

	comments, _ := fakeIssueComments(t, server, prNumber)
	marked := 0
	for _, comment := range comments {
		if isMergeReviewStatusComment(comment) {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("marked comments = %d, want 1: %q", marked, comments)
	}
	if comments[0] != humanComment {
		t.Fatalf("unrelated comment = %q, want byte-for-byte %q", comments[0], humanComment)
	}
}

func TestRetriedMergeReviewStatusCommentsConverge(t *testing.T) {
	const prNumber = 789
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "retried sticky status")
	const humanComment = "Automated note from another bot."
	server.addComment(prNumber, humanComment)
	server.addComment(prNumber, renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "first retry",
	}))
	server.addComment(prNumber, renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "duplicate retry",
	}))
	provider := server.newGitHubProvider("token")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	current := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Summary:  "canonical result",
	})

	if err := reconcileMergeReviewStatusComment(context.Background(), provider, repo, prNumber, current); err != nil {
		t.Fatalf("reconcile retries: %v", err)
	}
	comments, _ := fakeIssueComments(t, server, prNumber)
	assertCanonicalMergeReviewStatus(t, comments, current, humanComment)
}

func TestMergeReviewStatusCommentIgnoresSpoofedMarker(t *testing.T) {
	const prNumber = 790
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "spoofed sticky status")
	spoofed := mergeReviewStatusMarker + "\nThis comment belongs to an unrelated contributor."
	server.addCommentAs(prNumber, "mallory", spoofed)
	provider := server.newGitHubProvider("token")
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	first := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "trusted status",
	})

	if err := reconcileMergeReviewStatusComment(context.Background(), provider, repo, prNumber, first); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	comments, ids := fakeIssueComments(t, server, prNumber)
	assertCanonicalMergeReviewStatus(t, comments, first, spoofed)
	trustedID := markedCommentIDAfter(t, comments, ids, 1)

	second := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Summary:  "updated trusted status",
	})
	if err := reconcileMergeReviewStatusComment(context.Background(), provider, repo, prNumber, second); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	comments, ids = fakeIssueComments(t, server, prNumber)
	assertCanonicalMergeReviewStatus(t, comments, second, spoofed)
	if got := markedCommentIDAfter(t, comments, ids, 1); got != trustedID {
		t.Fatalf("trusted status comment id = %d, want original id %d", got, trustedID)
	}
}

func fakeIssueComments(t *testing.T, server *fakeGitHubServer, prNumber int) ([]string, []int64) {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	issue := server.issues[prNumber]
	if issue == nil {
		t.Fatalf("issue #%d not found", prNumber)
	}
	return append([]string(nil), issue.comments...), append([]int64(nil), issue.commentIDs...)
}

func assertCanonicalMergeReviewStatus(t *testing.T, comments []string, wantStatus, wantUnrelated string) {
	t.Helper()
	if len(comments) != 2 {
		t.Fatalf("comments = %q, want unrelated plus one status", comments)
	}
	if comments[0] != wantUnrelated {
		t.Fatalf("unrelated comment = %q, want byte-for-byte %q", comments[0], wantUnrelated)
	}
	if comments[1] != wantStatus {
		t.Fatalf("status comment = %q, want %q", comments[1], wantStatus)
	}
	if count := strings.Count(comments[1], mergeReviewStatusMarker); count != 1 {
		t.Fatalf("status marker count = %d, want 1", count)
	}
}

func markedCommentID(t *testing.T, comments []string, ids []int64) int64 {
	t.Helper()
	return markedCommentIDAfter(t, comments, ids, 0)
}

func markedCommentIDAfter(t *testing.T, comments []string, ids []int64, start int) int64 {
	t.Helper()
	for i, comment := range comments[start:] {
		if isMergeReviewStatusComment(comment) {
			return ids[start+i]
		}
	}
	t.Fatal("marked merge-review status comment not found")
	return 0
}
