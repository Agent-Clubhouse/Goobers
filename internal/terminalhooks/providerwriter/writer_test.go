package providerwriter

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/providers"
)

func TestPendingFailsEveryOperationClosed(t *testing.T) {
	ctx, w, ref := context.Background(), Pending{}, ItemRef{RunID: "r", ItemID: "1"}
	_, e1 := w.WorkItem(ctx, ref)
	_, e2 := w.Comments(ctx, ref)
	e3 := w.Update(ctx, ref, providers.UpdateWorkItemRequest{})
	e4 := w.UpdateComment(ctx, ref, "c", "b")
	_, e5 := w.PullLabels(ctx, "r", "2")
	e6 := w.DeleteBranch(ctx, "r", "b")
	e7 := w.ReleaseClaimMarker(ctx, ref, claimrelease.Reason{})
	for i, err := range []error{e1, e2, e3, e4, e5, e6, e7} {
		if !errors.Is(err, ErrPending) {
			t.Fatalf("op %d = %v, want ErrPending", i, err)
		}
		if Permanent(err) {
			t.Fatalf("op %d: pending must be retryable", i)
		}
	}
}

func TestPermanentOnlyForUnpinned(t *testing.T) {
	if !Permanent(fmt.Errorf("x: %w", ErrRepositoryNotPinned)) || !Permanent(fmt.Errorf("y: %w", ErrPermanent)) || Permanent(errors.New("transient")) {
		t.Fatal("Permanent misclassifies")
	}
}
