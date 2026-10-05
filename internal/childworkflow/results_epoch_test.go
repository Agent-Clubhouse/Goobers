package childworkflow

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func restartResultChild(t *testing.T, queue *triggerqueue.Store, child triggerqueue.ChildRecord, result TerminalResult) triggerqueue.ChildRecord {
	t.Helper()
	plan := []byte(`{"guidance":"retained human guidance"}`)
	runID := strings.Repeat("a", 32)
	if child.ExecutionEpoch > 0 {
		runID = strings.Repeat("b", 32)
	}
	_, _, err := queue.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: child.Identity, RunID: runID, SourceRunID: child.ActiveRunID(), SourceTerminalSeq: 11, SourceResultRef: result.ResultRef, Actor: "oidc:operator", Stage: "work", Plan: plan, PlanDigest: journal.Digest(plan)}, child.UpdatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	current, err := queue.GetChild(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func TestScratchHumanEpochPreservesSourceAndRejectsStaleObservation(t *testing.T) {
	service, authority, path := submissionFixture(t)
	accepted, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "result-epoch", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	c := WorkspaceCoordinator{Queue: service.Queue}
	original := accepted.Child
	input := TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: original.AcceptedAt.Add(time.Second), Summary: "original failure"}
	first, err := c.CaptureResult(t.Context(), original, nil, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Queue.SetChildState(t.Context(), original.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: first.ResultRef}, input.FinishedAt); err != nil {
		t.Fatal(err)
	}
	original, _ = service.Queue.GetChild(t.Context(), original.Identity)
	prior, err := service.Queue.ChildResult(t.Context(), original.Identity)
	if err != nil {
		t.Fatal(err)
	}
	current := restartResultChild(t, service.Queue, original, first)
	if err := service.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	service.Queue, err = triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Queue = service.Queue
	if _, err := c.ReadResult(t.Context(), original, ""); !errors.Is(err, triggerqueue.ErrChildResultUnavailable) {
		t.Fatal("stale current selector", err)
	}
	if _, err := c.CaptureResult(t.Context(), original, nil, input); !errors.Is(err, triggerqueue.ErrChildResultUnavailable) {
		t.Fatal("late prior observation", err)
	}
	retained, err := c.ReadExecutionResult(t.Context(), current, original.RunID, "")
	if err != nil || retained.ResultRef != first.ResultRef {
		t.Fatal(retained, err)
	}
	if _, err := c.ReadResult(t.Context(), current, ""); !errors.Is(err, triggerqueue.ErrChildResultPending) {
		t.Fatal("prior result satisfied new epoch", err)
	}
	input.State, input.Summary = triggerqueue.ChildCompleted, "human-guided result"
	input.FinishedAt = current.UpdatedAt.Add(time.Second)
	if err := service.Queue.SetChildState(t.Context(), current.Identity, triggerqueue.ChildStateUpdate{ExecutionRunID: current.ActiveRunID(), Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildRunning}, current.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	second, err := c.CaptureResult(t.Context(), current, nil, input)
	if err != nil || second.ResultRef == first.ResultRef {
		t.Fatal(second, err)
	}
	if err := service.Queue.SetChildState(t.Context(), current.Identity, triggerqueue.ChildStateUpdate{ExecutionRunID: current.ActiveRunID(), Expected: triggerqueue.ChildRunning, State: triggerqueue.ChildCompleted, ResultRef: second.ResultRef}, input.FinishedAt); err != nil {
		t.Fatal(err)
	}
	stored, err := service.Queue.ChildExecutionResult(t.Context(), current.Identity, original.RunID)
	if err != nil || !bytes.Equal(stored.Receipt, prior.Receipt) {
		t.Fatal("source custody changed", err)
	}
	if got, err := c.ReadResult(t.Context(), current, ""); err != nil || got.ResultRef != second.ResultRef {
		t.Fatal(got, err)
	}
}

func TestSealedEscalationCanSeedHumanEpoch(t *testing.T) {
	service, authority, _ := submissionFixture(t)
	accepted, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "escalated", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	child := accepted.Child
	if err := service.Queue.BeginDispatch(t.Context(), child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := service.Queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildRunning}, child.AcceptedAt); err != nil {
		t.Fatal(err)
	}
	if err := service.Queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildRunning, State: triggerqueue.ChildAwaitingHuman}, child.AcceptedAt); err != nil {
		t.Fatal(err)
	}
	child, _ = service.Queue.GetChild(t.Context(), child.Identity)
	c := WorkspaceCoordinator{Queue: service.Queue}
	result, err := c.CaptureResult(t.Context(), child, nil, TerminalResultInput{State: triggerqueue.ChildAwaitingHuman, FinishedAt: child.AcceptedAt.Add(time.Second), Summary: "escalated after writers joined"})
	if err != nil {
		t.Fatal(err)
	}
	current := restartResultChild(t, service.Queue, child, result)
	if got, err := c.ReadExecutionResult(t.Context(), current, child.RunID, ""); err != nil || got.Input.State != triggerqueue.ChildAwaitingHuman {
		t.Fatal(got, err)
	}
}
