package childworkflow

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestPreparedStageLeaseRechecksDurableCancellationBeforeDelivery(t *testing.T) {
	r, _, env := runtimeFixture(t)
	lease, err := r.AcquirePreparedStage(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := lease.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	parent := triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}
	if err := r.queue.FenceChildParent(t.Context(), parent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Verify(t.Context()); !errors.Is(err, triggerqueue.ErrParentCancelled) {
		t.Fatalf("cancelled attempt retained credential delivery: %v", err)
	}
	lease.Release()
	if next, err := r.AcquirePreparedStage(t.Context(), env); !errors.Is(err, triggerqueue.ErrParentCancelled) {
		if err == nil {
			next.Release()
		}
		t.Fatalf("cancelled parent reacquired authority: %v", err)
	}
}

func TestPreparedStageLeaseRechecksReplacementWithoutAllocatingCustody(t *testing.T) {
	r, f, env := runtimeFixture(t)
	parent := triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}
	lease, err := r.AcquirePreparedStage(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if exists, err := r.queue.IsChildParent(t.Context(), parent); err != nil || exists {
		t.Fatal("preflight allocated parent custody", exists, err)
	}
	f.start(t, 1, 2, true)
	if err := lease.Verify(t.Context()); err == nil {
		t.Fatal("replaced stage retained dispatch authority")
	}
}
