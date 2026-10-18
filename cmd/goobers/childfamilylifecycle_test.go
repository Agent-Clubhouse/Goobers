package main

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildFamilyCancellationFencesBeforeUnavailableRunnerDelivery(t *testing.T) {
	f := actualChildLaunchFixture(t)
	family := &childFamilyLifecycle{layout: f.launcher.layout, queue: f.service.queue, runners: f.launcher.runners}
	cancels := newDaemonCancelService(f.launcher.runners)
	cancels.fenceChildren = family.Fence
	e := f.submission.Envelope
	if _, err := cancels.Cancel(t.Context(), httpapi.CancelRunRequest{RunID: e.ParentRunID, Gaggle: "foreign", Actor: "human"}); err == nil {
		t.Fatal("foreign family cancelled")
	}
	child, _ := f.state(t)
	if child.CancellationRequested {
		t.Fatal("foreign cancellation crossed scope")
	}
	if _, err := cancels.Cancel(t.Context(), httpapi.CancelRunRequest{RunID: e.ParentRunID, Gaggle: e.Gaggle, Actor: "human"}); err != nil {
		t.Fatal(err)
	}
	child, record := f.state(t)
	if !child.CancellationRequested || record.State != triggerqueue.Rejected {
		t.Fatal("cancel delivery preceded durable fence")
	}
}

func TestChildFamilySweepPreservesEscalationAndClosesCompletedParent(t *testing.T) {
	f := actualChildLaunchFixture(t)
	f.service.childFamilies = &childFamilyLifecycle{layout: f.launcher.layout, queue: f.service.queue, runners: f.launcher.runners}
	// The production Drain caller runs family settlement before attempting any
	// new child dispatch, even without an installed child launcher/scheduler.
	f.service.children = nil
	f.service.dispatch.sched.Store(nil)
	dir, err := f.launcher.layout.FindRunDir(f.submission.Envelope.ParentRunID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	parents, err := f.service.queue.UnsettledChildParents(t.Context(), triggerqueue.ChildParent{}, 100)
	if err != nil || len(parents) != 1 {
		t.Fatalf("human escalation lost family: %v %v", parents, err)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunResumed}); err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	// Reset the cursor via a complete cycle before revisiting this parent.
	for range 2 {
		if err = f.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	parents, err = f.service.queue.UnsettledChildParents(t.Context(), triggerqueue.ChildParent{}, 100)
	if err != nil || len(parents) != 0 {
		t.Fatalf("terminal family stayed open: %v %v", parents, err)
	}
	child, record := f.state(t)
	if !child.CancellationRequested || record.State != triggerqueue.Rejected {
		t.Fatal("parent completed with runnable orphan child")
	}
}

func TestChildGenerationPinsFollowQueueCustodyUntilTombstone(t *testing.T) {
	f := newChildDrainFixture(t)
	retainer := &configgeneration.Retainer{}
	attachChildGenerationPins(retainer, f.service.queue)
	check := func(want bool) {
		t.Helper()
		pins, err := retainer.DurablePins(t.Context())
		if err != nil || pins[f.submission.Envelope.ConfigGeneration] != want {
			t.Fatalf("queue pins=%v err=%v", pins, err)
		}
	}
	check(true)
	child := f.submission.Child
	now := child.AcceptedAt.Add(time.Second)
	if err := f.service.queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildCancelled, ResultRef: "retained-result"}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.service.queue.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.queue.PruneChildren(t.Context(), now.Add(2*triggerqueue.ChildRetention), 100); err != nil {
		t.Fatal(err)
	}
	check(true) // unacknowledged family remains fully recoverable
	if err := f.service.queue.AcknowledgeChild(t.Context(), child.Identity, "retained-result", now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.queue.PruneChildren(t.Context(), now.Add(2*triggerqueue.ChildRetention), 100); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := f.service.queue.ChildProposal(t.Context(), child.Identity); !errors.Is(err, triggerqueue.ErrChildProposalUnavailable) {
		t.Fatalf("source survived tombstone: %v", err)
	}
}
