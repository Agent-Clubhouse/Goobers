package providers

import (
	"context"
	"testing"
)

// TestADOFeedbackThreadsKeepStatusAndEveryOpener (#6134): the thread read keeps
// general threads and threads the credential's own identity opened (a person's
// under shared identity), reports each thread's resolution, and applies the
// comment read's skip rules.
func TestADOFeedbackThreadsKeepStatusAndEveryOpener(t *testing.T) {
	general := adoThreadFixture{"id": 1, "status": "active", "comments": []map[string]interface{}{
		adoFeedbackComment(1, 0, "self-guid", "please add a README note", "2026-09-01T10:00:00Z"),
	}}
	resolved := adoFileThread(2, "fixed", "/a.go", 3, 0,
		adoFeedbackComment(1, 0, "reviewer-guid", "rename this", "2026-09-01T09:00:00Z"),
		deletedADOThreadComment(adoFeedbackComment(2, 1, "reviewer-guid", "", "2026-09-01T09:30:00Z")))
	synthesized := adoThreadFixture{
		"id":         3,
		"properties": map[string]interface{}{"CodeReviewThreadType": map[string]string{"$type": "System.String", "$value": "VoteUpdate"}},
		"comments":   []map[string]interface{}{adoFeedbackComment(1, 0, "reviewer-guid", "voted", "2026-09-01T11:00:00Z")},
	}
	provider := newADOFeedbackProvider(t, []adoThreadFixture{general, resolved, synthesized})

	got, err := provider.ListPullRequestFeedbackThreads(context.Background(), adoFeedbackRepo(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("threads = %+v, want the general and the file thread only", got)
	}
	if got[0].ID != "42/1" || got[0].Resolved || got[0].Path != "" || len(got[0].Comments) != 1 || got[0].Comments[0].AuthorID != "self-guid" {
		t.Fatalf("general thread = %+v, want an open, unanchored thread keeping the self-authored comment", got[0])
	}
	if got[1].ID != "42/2" || !got[1].Resolved || got[1].Path != "a.go" || len(got[1].Comments) != 1 {
		t.Fatalf("file thread = %+v, want a resolved a.go thread with its one live comment", got[1])
	}
}

func TestADOFeedbackThreadsFailClosedOnUnreadableComment(t *testing.T) {
	broken := adoFeedbackComment(1, 0, "", "no author id", "2026-09-01T10:00:00Z")
	provider := newADOFeedbackProvider(t, []adoThreadFixture{{"id": 1, "status": "active", "comments": []map[string]interface{}{broken}}})
	if _, err := provider.ListPullRequestFeedbackThreads(context.Background(), adoFeedbackRepo(), "42"); err == nil {
		t.Fatal("ListPullRequestFeedbackThreads succeeded on a comment with no author id, want a fail-closed error")
	}
}
