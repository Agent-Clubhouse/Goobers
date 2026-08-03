package gate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

type fakeCommenter struct {
	lastReq       providers.UpdateWorkItemRequest
	calls         int
	err           error
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
