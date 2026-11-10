package childengine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// capacityOne is a scheduler whose parent workflow admits one run at a time,
// with the parent already admitted.
func capacityOne(t *testing.T) (*localscheduler.Scheduler, string, func()) {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	s := localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: "parent", Gaggle: "own", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub},
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}}}, log)
	parent := strings.Repeat("a", 32)
	release, ok, reason := s.ReserveContinuation(parent, "own", "parent")
	if !ok {
		t.Fatal(reason)
	}
	return s, parent, release
}

// countedSlots counts suspensions: a running parent suspended again by a
// retry would lend its slot to anyone who asks before it resumes.
type countedSlots struct {
	*localscheduler.Scheduler
	suspends int
}

func (c *countedSlots) SuspendChildParent(ctx context.Context, runID string) (*localscheduler.ChildParentSuspension, error) {
	c.suspends++
	return c.Scheduler.SuspendChildParent(ctx, runID)
}

func parkOf(runID string) engine.ChildParkInput {
	return engine.ChildParkInput{Binding: engine.ChildRunBinding{Request: journal.ChildHandoffRequest{ParentRunID: runID}}}
}

func completionOf(runID string) engine.ChildCompletionInput {
	return engine.ChildCompletionInput{Binding: engine.ChildRunBinding{Request: journal.ChildHandoffRequest{ParentRunID: runID}}}
}

// With a single slot, a parked parent lends its slot so another run (its
// child's sibling work) can admit, and reacquires only once that run closes.
// A refusal leaves it parked as a retryable unavailability; retries and lost
// acknowledgements never release or mint a second slot.
func TestSchedulerCapacityLendsAndFairlyReacquiresTheParentSlot(t *testing.T) {
	s, parent, release := capacityOne(t)
	slots := &countedSlots{Scheduler: s}
	capacity := &SchedulerCapacity{Slots: slots}
	other := strings.Repeat("c", 32)
	if _, ok, _ := s.ReserveContinuation(other, "own", "parent"); ok {
		t.Fatal("a second run admitted beside the running parent")
	}
	for range 2 { // a retried park releases once
		if err := capacity.Release(t.Context(), parkOf(parent)); err != nil {
			t.Fatal(err)
		}
	}
	otherRelease, ok, reason := s.ReserveContinuation(other, "own", "parent")
	if !ok {
		t.Fatalf("the parked parent's slot was not lent: %s", reason)
	}
	if _, ok, _ := s.ReserveContinuation(strings.Repeat("d", 32), "own", "parent"); ok {
		t.Fatal("a retried park released a second slot")
	}
	err := capacity.Reacquire(t.Context(), completionOf(parent))
	if !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("reacquire over a full pool = %v, want unavailable (retry)", err)
	}
	otherRelease()
	for range 2 { // a lost acknowledgement retries the reacquire
		if err := capacity.Reacquire(t.Context(), completionOf(parent)); err != nil {
			t.Fatalf("reacquire with a free slot: %v", err)
		}
	}
	if _, ok, _ := s.ReserveContinuation(other, "own", "parent"); ok {
		t.Fatal("the resumed parent does not hold its slot, or a retry released it")
	}
	if slots.suspends != 2 {
		t.Fatalf("suspensions = %d, want only the two parks: a retried reacquire must resume the kept suspension", slots.suspends)
	}
	release()
	if _, ok, reason := s.ReserveContinuation(other, "own", "parent"); !ok {
		t.Fatalf("the parent's original cleanup did not return the resumed slot: %s", reason)
	}
}

// A parent this scheduler did not admit, or one whose admission ended while
// parked, is refused: lending or reacquiring capacity it does not hold would
// mint a slot.
func TestSchedulerCapacityRefusesAParentItDoesNotHold(t *testing.T) {
	s, parent, release := capacityOne(t)
	capacity := &SchedulerCapacity{Slots: s}
	if err := capacity.Release(t.Context(), parkOf(strings.Repeat("e", 32))); !errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("release of an unadmitted parent = %v, want refused", err)
	}
	if err := capacity.Release(t.Context(), parkOf(parent)); err != nil {
		t.Fatal(err)
	}
	release() // the parent is cancelled while parked
	if err := capacity.Reacquire(t.Context(), completionOf(parent)); !errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("reacquire after the parent's admission ended = %v, want refused", err)
	}
	if err := (&SchedulerCapacity{}).Release(t.Context(), parkOf(parent)); !errors.Is(err, engine.ErrChildCustodyUnavailable) || !errors.Is(err, ErrCapacityPlaneNotComposed) {
		t.Fatalf("release without a scheduler = %v", err)
	}
}
