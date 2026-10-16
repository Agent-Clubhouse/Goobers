package childworkflow

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestScratchTerminalResultIdentityAndImmutableObservation(t *testing.T) {
	service, authority, _ := submissionFixture(t)
	submission, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "result", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	c := WorkspaceCoordinator{Queue: service.Queue}
	input := TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: submission.Child.AcceptedAt.Add(time.Second), Summary: "Placement refused before execution"}
	result, err := c.CaptureResult(t.Context(), submission.Child, nil, input)
	if err != nil || result.ResultRef == "" || result.WorkspaceRef != "" {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := c.CaptureResult(t.Context(), submission.Child, nil, input); err != nil {
		t.Fatal(err)
	}
	input.Summary = "changed"
	if _, err := c.CaptureResult(t.Context(), submission.Child, nil, input); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal(err)
	}
	foreign := submission.Child
	foreign.RunID = "foreign"
	if _, err := c.ReadResult(t.Context(), foreign, ""); !errors.Is(err, ErrAuthorityUnavailable) {
		t.Fatal(err)
	}
	input.State = triggerqueue.ChildRunning
	if _, err := c.CaptureResult(t.Context(), submission.Child, nil, input); err == nil {
		t.Fatal("nonterminal result accepted")
	}
}
