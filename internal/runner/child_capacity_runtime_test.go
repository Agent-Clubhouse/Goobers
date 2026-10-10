package runner

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

type parallelRuntimeHandoff struct {
	waiting chan struct{}
	finish  chan struct{}
}

func (*parallelRuntimeHandoff) Await(context.Context, apiv1.InvocationEnvelope) (ChildHandoffRequest, error) {
	panic("invocation is already durably yielded")
}
func (*parallelRuntimeHandoff) Yield(context.Context, ChildHandoffRequest, ChildWorkspaceCustody) error {
	return nil
}
func (h *parallelRuntimeHandoff) Wait(ctx context.Context, _ ChildHandoffRequest) (ChildHandoffCompletion, error) {
	close(h.waiting)
	select {
	case <-h.finish:
		return ChildHandoffCompletion{Summary: "child complete"}, nil
	case <-ctx.Done():
		return ChildHandoffCompletion{}, ctx.Err()
	}
}

func TestParallelChildRuntimeWaitAccountsForSiblingAndPublishesContinuation(t *testing.T) {
	f := newParallelCapacityFixture(t)
	r, _, frame := childOriginRuntime(t, &childOriginGoober{})
	r.cfg.ChildParentCapacity = f.capacity
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: f.request.ParentRunID, Gaggle: "own"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	p := apiv1.Parallel{Name: "fan", Branches: []apiv1.Branch{{Name: "a", Start: "work"}, {Name: "b", Start: "work"}}}
	par := newParallelExec(p)
	if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: p.Name, Completeness: par.completeness()}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	frame.in.RunID, frame.in.Gaggle = f.request.ParentRunID, "own"
	frame.in.parallelChild, err = r.parallelChildOwner(frame.in, par, events)
	if err != nil {
		t.Fatal(err)
	}
	slots := newParallelBranchSlots(1)
	frame.in.parallelSlot, _ = slots.tryAcquire()
	frame.jr = &branchJournal{run: run, branch: 1}
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	request := ChildHandoffRequest{Gaggle: "own", ParentRunID: f.request.ParentRunID, RequestID: journal.Digest([]byte("request")), Action: "wait", ChildRunID: f.request.RunID, AcceptanceID: "trigger-" + f.request.RunID, InvocationKey: "child", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}
	record := childWaitRecord{Version: 1, ParentRunID: frame.in.RunID, Request: request}
	if _, err := r.suspendChildBranch(ctx, &frame, request); err == nil || len(slots.occupied) != 1 {
		t.Fatal("unpublished wait released branch capacity", err)
	}
	wait, err := childWaitEvent(frame.t.Name, 1, "", record)
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.jr.Append(wait); err != nil {
		t.Fatal(err)
	}
	wrong := request
	wrong.RequestID = journal.Digest([]byte("other"))
	if _, err := r.suspendChildBranch(ctx, &frame, wrong); err == nil || len(slots.occupied) != 1 {
		t.Fatal("wrong request released branch capacity", err)
	}
	handoff := &parallelRuntimeHandoff{waiting: make(chan struct{}), finish: make(chan struct{})}
	r.cfg.ChildHandoff = handoff
	done := make(chan error, 1)
	go func() { done <- r.continueChildWait(ctx, &frame, 1, "", record, &stageWorkspace{path: "owned-parent"}) }()
	// Always join before closing the journal, including failed assertions.
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	select {
	case <-handoff.waiting:
	case err := <-done:
		joined = true
		t.Fatal("parent did not reach child wait", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	sibling, ok := slots.tryAcquire()
	if !ok {
		t.Fatal("waiting branch retained its execution slot")
	}
	f.requireChildBlocked(t, localscheduler.ReasonMaxParallel)
	if err := finishParallelChildBranch(ctx, frame.in.parallelChild, 2, func() error {
		return settleConcurrentBranch(run, par, p.Name, parallelBranchResult{index: 1, status: journal.BranchSucceeded})
	}); err != nil {
		t.Fatal(err)
	}
	sibling.release()
	childDone := f.runChild(t)
	childDone()
	close(handoff.finish)
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("parent did not continue", ctx.Err())
	}
	if len(slots.occupied) != 1 || frame.heldChildWorkspace == nil || len(frame.upstream) != 1 {
		t.Fatal("continuation lost its execution slot, workspace, or child result")
	}
	frame.in.parallelSlot.release()
	f.requireChildBlocked(t, localscheduler.ReasonMaxParallel)
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	projection, err := journal.ProjectChildWaits(events)
	if err != nil || len(projection.Waits) != 0 {
		t.Fatal("continuation did not close exact durable wait", err)
	}
	var finishSeq, continueSeq uint64
	for _, event := range events {
		if event.Type == journal.EventBranchFinished && event.Branch == 2 {
			finishSeq = event.Seq
		}
		if event.Runner["kind"] == ChildContinuedKind && event.Branch == 1 {
			continueSeq = event.Seq
		}
	}
	if finishSeq == 0 || continueSeq <= finishSeq {
		t.Fatal("parent continued before sibling settled", finishSeq, continueSeq)
	}
}
