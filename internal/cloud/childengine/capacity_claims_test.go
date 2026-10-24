package childengine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// heldStarter keeps each started run going until the test finishes it.
type heldStarter struct{ finish chan struct{} }

func (h heldStarter) Start(ctx context.Context, _ localscheduler.StartRequest) (localscheduler.StartResult, error) {
	select {
	case <-h.finish:
	case <-ctx.Done():
	}
	return localscheduler.StartResult{}, nil
}

// The engine's Complete reaches the scheduler through SchedulerCapacity, so
// the parent's refused Reacquire is what claims the next freed slot (#8174):
// once a competing run ends, a new trigger is refused (retryably, as
// capacity) and the parent's retried Reacquire takes the slot.
func TestRefusedReacquireWinsTheNextSlotOverACompetingTrigger(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	starter := heldStarter{finish: make(chan struct{})}
	t.Cleanup(func() { close(starter.finish) })
	s := localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: "parent", Gaggle: "own", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub},
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 1000}, Starter: starter}}, log)
	parent := strings.Repeat("a", 32)
	release, ok, reason := s.ReserveContinuation(parent, "own", "parent")
	if !ok {
		t.Fatal(reason)
	}
	defer release()
	capacity := &SchedulerCapacity{Slots: s}

	if err := capacity.Release(t.Context(), parkOf(parent)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Trigger(t.Context(), "parent", time.Now()); err != nil { // the lent slot, by design
		t.Fatalf("a run on the parked parent's lent slot: %v", err)
	}
	if err := capacity.Reacquire(t.Context(), completionOf(parent)); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("reacquire over a full pool = %v, want unavailable (the engine retries)", err)
	}
	starter.finish <- struct{}{} // the competing run ends
	s.Wait()                     // and its dispatch has released its slot
	_, err = s.Trigger(t.Context(), "parent", time.Now())
	var refused *localscheduler.TriggerRejectedError
	if !errors.As(err, &refused) || refused.Reason != localscheduler.ReasonMaxParallel || !refused.Transient() {
		t.Fatalf("a new trigger while the parent waits = %v, want a retryable capacity refusal", err)
	}
	if err := capacity.Reacquire(t.Context(), completionOf(parent)); err != nil {
		t.Fatalf("the parent's retried reacquire = %v, want the freed slot", err)
	}
	if _, err := s.Trigger(t.Context(), "parent", time.Now()); !errors.As(err, &refused) || refused.Reason != localscheduler.ReasonMaxParallel {
		t.Fatalf("a trigger after the parent resumed = %v, want refused: the parent holds the slot", err)
	}
}
