package main

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/childengine"
	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
)

func parkInput(runID string) engine.ChildParkInput {
	return engine.ChildParkInput{Binding: engine.ChildRunBinding{Request: journal.ChildHandoffRequest{ParentRunID: runID}}}
}

func completionInput(runID string) engine.ChildCompletionInput {
	return engine.ChildCompletionInput{Binding: engine.ChildRunBinding{Request: journal.ChildHandoffRequest{ParentRunID: runID}}}
}

// With no scheduler published (standby, before Prepare, after Stop) Park and
// Complete refuse as not composed, never as a nil dereference.
func TestParentCapacityFailsClosedWhenNothingIsPublished(t *testing.T) {
	var holder parentCapacity
	for name, err := range map[string]error{
		"release":   holder.Release(t.Context(), parkInput("run-1")),
		"reacquire": holder.Reacquire(t.Context(), completionInput("run-1")),
	} {
		if !errors.Is(err, childengine.ErrCapacityPlaneNotComposed) || !errors.Is(err, engine.ErrChildCustodyUnavailable) {
			t.Fatalf("%s = %v; want the not-composed, unavailable refusal", name, err)
		}
	}
}

// A late retire by a deposed epoch must not clear the capacity its successor
// published.
func TestParentCapacityRetireOnlyClearsItsOwn(t *testing.T) {
	var holder parentCapacity
	first, second := &childengine.SchedulerCapacity{}, &childengine.SchedulerCapacity{}
	holder.publish(first)
	holder.publish(second)
	holder.retire(first)
	if holder.current.Load() != second {
		t.Fatal("the deposed epoch's retire cleared its successor's capacity")
	}
	holder.retire(second)
	if holder.current.Load() != nil {
		t.Fatal("the current epoch's retire left its capacity published")
	}
}

// Park after a takeover reaches the SUCCESSOR's scheduler, and the successor's
// capacity carries no suspension cached by the predecessor. Behavioral proof: the
// deposed epoch parked run 1 on its own scheduler (lending its slot, so run 2
// started). The successor, with run 1 reconciled as parked and a free slot,
// resumes it; had the predecessor's cached suspension been reused, the resume
// would have hit the predecessor's scheduler, where run 2 holds the only slot,
// and been refused.
func TestParentCapacityFollowsTheCurrentEpochsScheduler(t *testing.T) {
	var holder parentCapacity

	fa := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, false)
	fa.comp.Capacity = &holder
	epochA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	a := fa.build(epochA)
	if err := a.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	fa.accept(2, "k")
	if err := a.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	run1 := <-fa.started
	fa.expectNoStart("the second run waits for the only slot")

	// Park on the first epoch: lends run 1's slot, so run 2 starts.
	if err := holder.Release(t.Context(), parkInput(run1)); err != nil {
		t.Fatalf("park on the live scheduler: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for !started && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if err := a.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-fa.started:
			started = true
		default:
		}
	}
	if !started {
		t.Fatal("the parked parent's slot was not lent through the published capacity")
	}

	// Takeover: the successor reconciles run 1 as parked, with no other holder.
	fb := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, false)
	fb.comp.Capacity = &holder
	fb.open.Store(&[]schedstart.OpenRun{{Identity: testIdentity, RunID: run1, Suspended: true}})
	epochB, cancelB := context.WithCancel(t.Context())
	defer cancelB()
	b := fb.build(epochB)
	if err := b.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if holder.current.Load() != b.capacity {
		t.Fatal("the successor's capacity is not the published one")
	}
	if err := holder.Reacquire(t.Context(), completionInput(run1)); err != nil {
		t.Fatalf("resume on the successor's scheduler: %v (the predecessor's cached suspension leaked in?)", err)
	}

	// The deposed epoch's late Stop leaves the successor's capacity alone; the
	// successor's own Stop withdraws it.
	cancelA()
	stopAdmission(t, a)
	if holder.current.Load() != b.capacity {
		t.Fatal("the deposed epoch's Stop cleared its successor's capacity")
	}
	cancelB()
	stopAdmission(t, b)
	if err := holder.Release(t.Context(), parkInput(run1)); !errors.Is(err, childengine.ErrCapacityPlaneNotComposed) {
		t.Fatalf("after the last epoch stopped, Park = %v; want not composed", err)
	}
}
