package gate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

type fakeStatusCommenter struct {
	lastReq providers.UpdateWorkItemStatusRequest
	calls   int
	err     error
}

func (f *fakeStatusCommenter) UpdateWorkItemStatus(_ context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return providers.WorkItem{}, f.err
	}
	return providers.WorkItem{}, nil
}

func TestNotifyEscalatedPostsComment(t *testing.T) {
	poster := &fakeStatusCommenter{}
	n := &EscalationNotifier{
		Poster:        poster,
		Repository:    providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"},
		CurrentStatus: providers.WorkItemStatusInProgress,
	}

	r := Result{Gate: "autogate", Attempt: 3, Outcome: OutcomeFail, Target: "@escalate", Escalated: true}
	if err := n.NotifyEscalated(context.Background(), "42", r, "repass budget exhausted"); err != nil {
		t.Fatalf("NotifyEscalated: %v", err)
	}
	if poster.calls != 1 {
		t.Fatalf("calls = %d, want 1", poster.calls)
	}
	if poster.lastReq.ID != "42" || poster.lastReq.Status != providers.WorkItemStatusInProgress {
		t.Fatalf("request = %+v, want id=42 status preserved", poster.lastReq)
	}
	if !strings.Contains(poster.lastReq.Comment, "autogate") || !strings.Contains(poster.lastReq.Comment, "repass budget exhausted") {
		t.Fatalf("comment = %q, want it to mention the gate and reason", poster.lastReq.Comment)
	}
}

func TestNotifyEscalatedNoopWithoutPosterOrItem(t *testing.T) {
	poster := &fakeStatusCommenter{}
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
	poster := &fakeStatusCommenter{err: errors.New("rate limited")}
	n := &EscalationNotifier{Poster: poster, Repository: providers.RepositoryRef{Name: "widgets"}}
	if err := n.NotifyEscalated(context.Background(), "42", Result{}, "why"); err == nil {
		t.Fatal("want error propagated from provider")
	}
}
