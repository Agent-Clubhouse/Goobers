package scheduler

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestBacklogClaimerMarksOpenItem(t *testing.T) {
	fb := &fakeBacklog{}
	c := BacklogClaimer{Provider: fb, Repo: providers.RepositoryRef{Provider: providers.ProviderGitHub, Name: "web"}}

	err := c.Claim(context.Background(), providers.WorkItem{ID: "42", Status: providers.WorkItemStatusOpen})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(fb.updated) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(fb.updated))
	}
	if fb.updated[0].ID != "42" || fb.updated[0].Status != providers.WorkItemStatusClaimed {
		t.Errorf("unexpected update: %+v", fb.updated[0])
	}
}

func TestBacklogClaimerIdempotent(t *testing.T) {
	fb := &fakeBacklog{}
	c := BacklogClaimer{Provider: fb}
	for _, st := range []providers.WorkItemStatus{
		providers.WorkItemStatusClaimed,
		providers.WorkItemStatusInProgress,
		providers.WorkItemStatusDone,
		providers.WorkItemStatusClosed,
	} {
		if err := c.Claim(context.Background(), providers.WorkItem{ID: "1", Status: st}); err != nil {
			t.Fatalf("Claim(%s): %v", st, err)
		}
	}
	if len(fb.updated) != 0 {
		t.Errorf("already-claimed items must not be re-updated, got %d updates", len(fb.updated))
	}
}

func TestBacklogClaimerNilProvider(t *testing.T) {
	if err := (BacklogClaimer{}).Claim(context.Background(), providers.WorkItem{ID: "1"}); err != nil {
		t.Errorf("nil provider should be a no-op, got: %v", err)
	}
}
