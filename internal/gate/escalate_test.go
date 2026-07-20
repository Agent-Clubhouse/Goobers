package gate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

type fakeCommenter struct {
	lastReq providers.UpdateWorkItemRequest
	calls   int
	err     error
}

func (f *fakeCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return providers.WorkItem{}, f.err
	}
	return providers.WorkItem{}, nil
}

func TestNotifyEscalatedPostsComment(t *testing.T) {
	poster := &fakeCommenter{}
	n := &EscalationNotifier{
		Poster:     poster,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"},
	}

	r := Result{Gate: "autogate", Attempt: 3, Outcome: OutcomeFail, Target: "@escalate", Escalated: true}
	if err := n.NotifyEscalated(context.Background(), "42", r, "repass budget exhausted"); err != nil {
		t.Fatalf("NotifyEscalated: %v", err)
	}
	if poster.calls != 1 {
		t.Fatalf("calls = %d, want 1", poster.calls)
	}
	if poster.lastReq.ID != "42" {
		t.Fatalf("request = %+v, want id=42", poster.lastReq)
	}
	if poster.lastReq.Title != nil || poster.lastReq.Body != nil || poster.lastReq.State != "" || len(poster.lastReq.AddLabels) != 0 || len(poster.lastReq.RemoveLabels) != 0 {
		t.Fatalf("request = %+v, want comment-only (no other field touched)", poster.lastReq)
	}
	if !strings.Contains(poster.lastReq.Comment, "autogate") || !strings.Contains(poster.lastReq.Comment, "repass budget exhausted") {
		t.Fatalf("comment = %q, want it to mention the gate and reason", poster.lastReq.Comment)
	}
}

func TestNotifyStageEscalatedPostsComment(t *testing.T) {
	poster := &fakeCommenter{}
	n := &EscalationNotifier{
		Poster:     poster,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"},
	}

	if err := n.NotifyStageEscalated(context.Background(), "42", "implement", "blocked on issue 41"); err != nil {
		t.Fatalf("NotifyStageEscalated: %v", err)
	}
	if poster.calls != 1 {
		t.Fatalf("calls = %d, want 1", poster.calls)
	}
	if poster.lastReq.ID != "42" {
		t.Fatalf("request = %+v, want id=42", poster.lastReq)
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
	if err := (&EscalationNotifier{Poster: nil}).NotifyEscalated(context.Background(), "42", Result{}, "why"); err != nil {
		t.Fatalf("nil poster: %v", err)
	}
	n := &EscalationNotifier{Poster: poster}
	if err := n.NotifyEscalated(context.Background(), "", Result{}, "why"); err != nil {
		t.Fatalf("empty item id: %v", err)
	}
	if poster.calls != 0 {
		t.Fatalf("calls = %d, want 0 (no-op cases)", poster.calls)
	}
}

func TestNotifyEscalatedPropagatesProviderError(t *testing.T) {
	poster := &fakeCommenter{err: errors.New("rate limited")}
	n := &EscalationNotifier{Poster: poster, Repository: providers.RepositoryRef{Name: "widgets"}}
	if err := n.NotifyEscalated(context.Background(), "42", Result{}, "why"); err == nil {
		t.Fatal("want error propagated from provider")
	}
}
