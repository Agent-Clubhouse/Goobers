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
	c := WorkspaceCoordinator{Queue: service.Queue.(*triggerqueue.Store)}
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

// The workspace-less capture an engine child's finalizer uses retains the
// receipt the coordinator retains with no workspace: each reads the other's
// result, neither carries a bundle or workspace, and a changed retry
// conflicts.
func TestCaptureTerminalResultMatchesTheCoordinatorWithoutAWorkspace(t *testing.T) {
	child := func(t *testing.T) (*triggerqueue.Store, triggerqueue.ChildRecord) {
		service, authority, _ := submissionFixture(t)
		submission, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "result", Source: []byte(validProposal)})
		if err != nil {
			t.Fatal(err)
		}
		return service.Queue.(*triggerqueue.Store), submission.Child
	}
	input := func(c triggerqueue.ChildRecord) TerminalResultInput {
		return TerminalResultInput{State: triggerqueue.ChildCompleted, FinishedAt: c.AcceptedAt.Add(time.Second), Summary: "done", References: []string{"sha256:abc"}}
	}

	store, viaCoordinator := child(t)
	first, err := (&WorkspaceCoordinator{Queue: store}).CaptureResult(t.Context(), viaCoordinator, nil, input(viaCoordinator))
	if err != nil {
		t.Fatal(err)
	}
	if read, err := ReadTerminalResult(t.Context(), store, viaCoordinator); err != nil || read.ResultRef != first.ResultRef || read.Input.Summary != "done" {
		t.Fatalf("ReadTerminalResult of the coordinator's result = %+v, %v", read, err)
	}
	if again, err := CaptureTerminalResult(t.Context(), store, viaCoordinator, input(viaCoordinator)); err != nil || again.ResultRef != first.ResultRef {
		t.Fatalf("capture over the coordinator's identical result = %+v, %v", again, err)
	}

	store, viaCapture := child(t)
	second, err := CaptureTerminalResult(t.Context(), store, viaCapture, input(viaCapture))
	if err != nil || second.WorkspaceRef != "" {
		t.Fatalf("capture = %+v, %v", second, err)
	}
	if read, err := (&WorkspaceCoordinator{Queue: store}).ReadResult(t.Context(), viaCapture, ""); err != nil || read.ResultRef != second.ResultRef {
		t.Fatalf("coordinator read of the capture's result = %+v, %v", read, err)
	}
	stored, err := store.ChildResult(t.Context(), viaCapture.Identity)
	if err != nil || len(stored.Bundle) != 0 || stored.BundleDigest != "" {
		t.Fatalf("workspace-less result carries a bundle: %+v, %v", stored, err)
	}
	if again, err := CaptureTerminalResult(t.Context(), store, viaCapture, input(viaCapture)); err != nil || again.ResultRef != second.ResultRef {
		t.Fatalf("repeated capture = %+v, %v", again, err)
	}
	changed := input(viaCapture)
	changed.Summary = "changed"
	if _, err := CaptureTerminalResult(t.Context(), store, viaCapture, changed); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("changed capture = %v, want ErrConflict", err)
	}

	store, pending := child(t)
	running := input(pending)
	running.State = triggerqueue.ChildRunning
	if _, err := CaptureTerminalResult(t.Context(), store, pending, running); err == nil {
		t.Fatal("a nonterminal result was captured")
	}
}
