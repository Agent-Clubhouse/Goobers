package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

type fakeCommenter struct {
	lastReq       providers.UpdateWorkItemRequest
	calls         int
	err           error
	updateErr     error
	commitOnError bool
	comments      []providers.Comment
}

func (f *fakeCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.calls++
	f.lastReq = req
	if f.err == nil || f.commitOnError {
		f.comments = append(f.comments, providers.Comment{Body: req.Comment})
	}
	if f.err != nil {
		return providers.WorkItem{}, f.err
	}
	return providers.WorkItem{}, nil
}

func (f *fakeCommenter) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return append([]providers.Comment(nil), f.comments...), nil
}

func (f *fakeCommenter) UpdateComment(_ context.Context, _ providers.RepositoryRef, commentID, body string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	for i, c := range f.comments {
		if c.ID == commentID {
			f.comments[i].Body = body
			return nil
		}
	}
	return fmt.Errorf("comment %s not found", commentID)
}

func TestNotifyEscalatedPostsComment(t *testing.T) {
	poster := &fakeCommenter{}
	n := &EscalationNotifier{Poster: poster}
	repository := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}

	r := Result{Gate: "autogate", Attempt: 3, Outcome: OutcomeFail, Target: "@escalate", Escalated: true}
	if err := n.NotifyEscalated(context.Background(), repository, "42", "run-42", 9, r, "repass budget exhausted"); err != nil {
		t.Fatalf("NotifyEscalated: %v", err)
	}
	if poster.calls != 1 {
		t.Fatalf("calls = %d, want 1", poster.calls)
	}
	if poster.lastReq.ID != "42" {
		t.Fatalf("request = %+v, want id=42", poster.lastReq)
	}
	if poster.lastReq.Repository != repository {
		t.Fatalf("repository = %+v, want %+v", poster.lastReq.Repository, repository)
	}
	if poster.lastReq.Title != nil || poster.lastReq.Body != nil || poster.lastReq.State != "" || len(poster.lastReq.AddLabels) != 0 || len(poster.lastReq.RemoveLabels) != 0 {
		t.Fatalf("request = %+v, want comment-only (no other field touched)", poster.lastReq)
	}
	if !strings.Contains(poster.lastReq.Comment, "autogate") || !strings.Contains(poster.lastReq.Comment, "repass budget exhausted") {
		t.Fatalf("comment = %q, want it to mention the gate and reason", poster.lastReq.Comment)
	}
	if !strings.Contains(poster.lastReq.Comment, "run=run-42 seq=9") {
		t.Fatalf("comment = %q, want run+seq marker", poster.lastReq.Comment)
	}
}

func TestNotifyStageEscalatedPostsComment(t *testing.T) {
	poster := &fakeCommenter{}
	n := &EscalationNotifier{Poster: poster}
	repository := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}

	if err := n.NotifyStageEscalated(context.Background(), repository, "42", "run-42", 10, "implement", "blocked on issue 41"); err != nil {
		t.Fatalf("NotifyStageEscalated: %v", err)
	}
	if poster.calls != 1 {
		t.Fatalf("calls = %d, want 1", poster.calls)
	}
	if poster.lastReq.ID != "42" {
		t.Fatalf("request = %+v, want id=42", poster.lastReq)
	}
	if poster.lastReq.Repository != repository {
		t.Fatalf("repository = %+v, want %+v", poster.lastReq.Repository, repository)
	}
	if poster.lastReq.Title != nil || poster.lastReq.Body != nil || poster.lastReq.State != "" || len(poster.lastReq.AddLabels) != 0 || len(poster.lastReq.RemoveLabels) != 0 {
		t.Fatalf("request = %+v, want comment-only (no other field touched)", poster.lastReq)
	}
	if !strings.Contains(poster.lastReq.Comment, "implement") || !strings.Contains(poster.lastReq.Comment, "blocked on issue 41") {
		t.Fatalf("comment = %q, want it to mention the stage and reason", poster.lastReq.Comment)
	}
}

func TestNotifyEscalatedNoopWithoutPosterOrItem(t *testing.T) {
	poster := &fakeCommenter{}
	if err := (&EscalationNotifier{Poster: nil}).NotifyEscalated(context.Background(), providers.RepositoryRef{}, "42", "run", 1, Result{}, "why"); err != nil {
		t.Fatalf("nil poster: %v", err)
	}
	n := &EscalationNotifier{Poster: poster}
	if err := n.NotifyEscalated(context.Background(), providers.RepositoryRef{}, "", "run", 1, Result{}, "why"); err != nil {
		t.Fatalf("empty item id: %v", err)
	}
	if poster.calls != 0 {
		t.Fatalf("calls = %d, want 0 (no-op cases)", poster.calls)
	}
}

func TestNotifyEscalatedPropagatesProviderError(t *testing.T) {
	poster := &fakeCommenter{err: errors.New("rate limited")}
	n := &EscalationNotifier{Poster: poster}
	if err := n.NotifyEscalated(context.Background(), providers.RepositoryRef{Name: "widgets"}, "42", "run", 1, Result{}, "why"); err == nil {
		t.Fatal("want error propagated when the comment was not committed")
	}
}

func TestPostRunCommentReconcilesCommittedLostResponse(t *testing.T) {
	poster := &fakeCommenter{err: errors.New("response lost"), commitOnError: true}
	repository := providers.RepositoryRef{Name: "widgets"}

	if err := PostRunComment(context.Background(), poster, repository, "42", "run-lost", 11, "failed"); err != nil {
		t.Fatalf("PostRunComment: %v", err)
	}
	poster.err = nil
	if err := PostRunComment(context.Background(), poster, repository, "42", "run-lost", 11, "failed"); err != nil {
		t.Fatalf("PostRunComment replay: %v", err)
	}
	if poster.calls != 1 {
		t.Fatalf("POST calls = %d, want 1 after reconciliation and replay", poster.calls)
	}
}

func TestPostRunCommentReconcilesMarkerWithDifferentVisibleText(t *testing.T) {
	poster := &fakeCommenter{
		comments: []providers.Comment{{
			Body: "original failure text\n\n" + runCommentMarker("run-revised", 12),
		}},
	}
	repository := providers.RepositoryRef{Name: "widgets"}

	if err := PostRunComment(context.Background(), poster, repository, "42", "run-revised", 12, "revised failure text"); err != nil {
		t.Fatalf("PostRunComment: %v", err)
	}
	if poster.calls != 0 {
		t.Fatalf("POST calls = %d, want 0 when the run+seq marker already exists", poster.calls)
	}
}

func TestUpsertFailureCommentCreatesWhenNoneExists(t *testing.T) {
	poster := &fakeCommenter{}
	if err := UpsertFailureComment(context.Background(), poster, providers.RepositoryRef{Name: "r"}, "1", 1, "implement", "run-1", "http://127.0.0.1:8080/#/run/run-1", nil); err != nil {
		t.Fatal(err)
	}
	if poster.calls != 1 {
		t.Fatalf("calls = %d, want 1", poster.calls)
	}
	if !strings.Contains(poster.comments[0].Body, failureStreakMarker) {
		t.Fatal("posted comment missing streak marker")
	}
	if !strings.Contains(poster.comments[0].Body, "[`run-1`](http://127.0.0.1:8080/#/run/run-1)") {
		t.Fatalf("posted comment missing run-details link: %s", poster.comments[0].Body)
	}
	if !strings.Contains(poster.comments[0].Body, "Classification: **genuine/work failure**") {
		t.Fatalf("posted comment missing failure classification: %s", poster.comments[0].Body)
	}
	if !strings.Contains(poster.comments[0].Body, "infra/transient failures are excluded") {
		t.Fatalf("posted comment missing infra exclusion: %s", poster.comments[0].Body)
	}
}

func TestUpsertFailureCommentEditsExisting(t *testing.T) {
	poster := &fakeCommenter{
		comments: []providers.Comment{
			{ID: "42", Body: failureStreakBody(1, "implement", "run-old", "http://127.0.0.1:8080/#/run/run-old", failurePark{})},
		},
	}

	if err := UpsertFailureComment(context.Background(), poster, providers.RepositoryRef{Name: "r"}, "1", 2, "implement", "run-new", "http://127.0.0.1:8080/#/run/run-new", nil); err != nil {
		t.Fatal(err)
	}
	if poster.calls != 0 {
		t.Fatalf("calls = %d, want 0 (should edit, not post)", poster.calls)
	}
	if !strings.Contains(poster.comments[0].Body, `data-count="2"`) {
		t.Fatalf("edited comment has wrong count: %s", poster.comments[0].Body)
	}
	if !strings.Contains(poster.comments[0].Body, "[`run-new`](http://127.0.0.1:8080/#/run/run-new)") {
		t.Fatalf("edited comment has stale run-details link: %s", poster.comments[0].Body)
	}
}

func TestUpsertFailureCommentFallsBackWhenEditingUnsupported(t *testing.T) {
	poster := &fakeCommenter{
		updateErr: errors.New("comment editing unsupported"),
		comments: []providers.Comment{
			{ID: "42", Body: failureStreakBody(1, "", "run-1", "http://run-1", failurePark{})},
		},
	}
	latestComment := func() string { return poster.comments[len(poster.comments)-1].Body }
	if !strings.Contains(latestComment(), `data-count="1"`) {
		t.Fatalf("latest comment before updates = %q, want data-count=\"1\"", latestComment())
	}
	for want := 2; want <= 3; want++ {
		if err := UpsertFailureComment(context.Background(), poster, providers.RepositoryRef{Provider: providers.ProviderADO, Name: "r"}, "1", want, "", fmt.Sprintf("run-%d", want), "http://run", nil); err != nil {
			t.Fatal(err)
		}
	}
	want := `data-count="3"`
	if !strings.Contains(latestComment(), want) {
		t.Fatalf("latest comment after three failures = %q, want %s", latestComment(), want)
	}
}

// labeledCommenter is a fakeCommenter that also implements WorkItemReader.
type labeledCommenter struct {
	fakeCommenter
	labels  []string
	readErr error
}

func (f *labeledCommenter) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	if f.readErr != nil {
		return providers.WorkItem{}, f.readErr
	}
	return providers.WorkItem{Labels: f.labels}, nil
}

// TestUpsertFailureCommentNamesActualParkLabel covers #5430: the retry
// instruction names the human-park label that actually holds the item, never
// a fixed needs-human, and hedges instead of guessing when labels are unknown.
func TestUpsertFailureCommentNamesActualParkLabel(t *testing.T) {
	cases := []struct {
		name     string
		poster   Commenter
		applying []string
		want     string
		absent   []string
	}{
		{
			name:   "merge-review escalation names merge-escalated",
			poster: &labeledCommenter{labels: []string{"goobers:merge-escalated"}},
			want:   "Remove `goobers:merge-escalated` and re-approve to retry.",
			absent: []string{providers.LabelNeedsHuman},
		},
		{
			name:     "circuit breaker park on an escalated PR names both",
			poster:   &labeledCommenter{labels: []string{"goobers:merge-escalated"}},
			applying: []string{providers.LabelNeedsHuman},
			want:     "Remove `goobers:needs-human` and `goobers:merge-escalated` and re-approve to retry.",
		},
		{
			name:     "circuit breaker park names needs-human",
			poster:   &labeledCommenter{labels: []string{providers.LabelReady}},
			applying: []string{providers.LabelNeedsHuman},
			want:     "Remove `goobers:needs-human` and re-approve to retry.",
			absent:   []string{providers.LabelMergeEscalated},
		},
		{
			name:   "item label casing is preserved",
			poster: &labeledCommenter{labels: []string{"Goobers:Needs-Human"}},
			want:   "Remove `Goobers:Needs-Human` and re-approve to retry.",
		},
		{
			name:   "unparked item needs no label removal",
			poster: &labeledCommenter{labels: []string{providers.LabelReady, providers.LabelNeedsRemediation}},
			want:   "No human park label (`goobers:needs-human` and `goobers:merge-escalated`) is on this item, so no label removal is needed to retry.",
		},
		{
			name:   "unreadable labels hedge instead of naming one",
			poster: &labeledCommenter{readErr: errors.New("boom")},
			want:   "If a human park label (`goobers:needs-human` and `goobers:merge-escalated`) is on this item, remove it and re-approve to retry.",
		},
		{
			name:   "poster without a reader hedges",
			poster: &fakeCommenter{},
			want:   "If a human park label",
		},
		{
			name:     "unreadable labels keep the applied park and hedge the rest",
			poster:   &labeledCommenter{readErr: errors.New("boom")},
			applying: []string{providers.LabelNeedsHuman},
			want:     "Remove `goobers:needs-human` (and any other human park label on this item: `goobers:needs-human` and `goobers:merge-escalated`) and re-approve to retry.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := UpsertFailureComment(context.Background(), tc.poster, providers.RepositoryRef{Name: "r"}, "7", 1, "park-review", "run-1", "http://run-1", tc.applying); err != nil {
				t.Fatal(err)
			}
			comments, err := tc.poster.ListComments(context.Background(), providers.RepositoryRef{Name: "r"}, "7")
			if err != nil || len(comments) != 1 {
				t.Fatalf("comments = %v, %v; want one", comments, err)
			}
			body := comments[0].Body
			if !strings.Contains(body, tc.want) {
				t.Fatalf("comment = %q, want it to contain %q", body, tc.want)
			}
			for _, label := range tc.absent {
				if strings.Contains(body, label) {
					t.Fatalf("comment = %q, must not name %q", body, label)
				}
			}
			if got, ok := ParseFailureStreakCount(body); !ok || got != 1 {
				t.Fatalf("ParseFailureStreakCount = %d, %v; want 1, true", got, ok)
			}
		})
	}
}

func TestResetFailureCommentResetsExistingMarker(t *testing.T) {
	poster := &fakeCommenter{
		comments: []providers.Comment{
			{ID: "42", Body: failureStreakBody(3, "", "run-fail", "http://run-fail", failurePark{})},
		},
	}
	if err := ResetFailureComment(context.Background(), poster, providers.RepositoryRef{Name: "r"}, "1", "run-ok", "http://run-ok"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(poster.comments[0].Body, `data-count="0"`) {
		t.Fatalf("comment after reset = %q, want data-count=\"0\"", poster.comments[0].Body)
	}
	if len(poster.comments) != 1 {
		t.Fatalf("comments = %d, want one rolling comment", len(poster.comments))
	}
}

func TestResetFailureCommentNoopsWithoutMarker(t *testing.T) {
	poster := &fakeCommenter{}
	if err := ResetFailureComment(context.Background(), poster, providers.RepositoryRef{Name: "r"}, "1", "run-ok", "http://run-ok"); err != nil {
		t.Fatal(err)
	}
	if poster.calls != 0 {
		t.Fatalf("calls = %d, want 0 when no failure marker exists", poster.calls)
	}
	if len(poster.comments) != 0 {
		t.Fatalf("comments = %d, want no comments", len(poster.comments))
	}
}

// TestParseFailureStreakCount covers Goobers#3025's migration-on-read input:
// extracting the legacy count from a real marker, and refusing to fabricate a
// count from anything else.
func TestParseFailureStreakCount(t *testing.T) {
	if got, ok := ParseFailureStreakCount(failureStreakBody(5, "implement", "run-1", "http://run-1", failurePark{})); !ok || got != 5 {
		t.Fatalf("ParseFailureStreakCount(real marker) = %d, %v; want 5, true", got, ok)
	}
	for _, body := range []string{
		"",
		"no marker here at all",
		"<!-- goobers:failure-streak -->",
		`<!-- goobers:failure-streak data-count="" -->`,
		`<!-- goobers:failure-streak data-count="not-a-number" -->`,
		`<!-- goobers:failure-streak data-count="-1" -->`,
	} {
		if got, ok := ParseFailureStreakCount(body); ok {
			t.Fatalf("ParseFailureStreakCount(%q) = %d, true; want false", body, got)
		}
	}
}
