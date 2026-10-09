package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// Exercise the real dispatcher: a live waiting branch must stop consuming a
// branch slot without becoming a finished branch, and its continuation must
// wait until a running sibling releases the bounded slot.
func TestParallelDispatchWaitReleasesSlotWithoutSettlingBranch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	slots := newParallelBranchSlots(1)
	results := make(chan parallelBranchResult, 2)
	started := make(chan *parallelBranchSlot, 2)
	done := make(chan error, 1)
	finish := []chan struct{}{make(chan struct{}), make(chan struct{})}
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "parent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	spec := apiv1.Parallel{Name: "fan", Branches: []apiv1.Branch{{Name: "a", Start: "a"}, {Name: "b", Start: "b"}}}
	par := newParallelExec(spec)
	_, cancelCause := context.WithCancelCause(ctx)
	dispatch := &parallelDispatch{queue: []int{0, 1}, outcomes: make([]*parallelBranchResult, 2), results: results, released: slots.changed, cancel: cancelCause, failurePolicy: apiv1.BranchContinueOnError}
	dispatch.settle = func(result parallelBranchResult) error { return settleConcurrentBranch(run, par, "fan", result) }
	dispatch.cancelQueued = func() error { return errors.New("unexpected branch cancellation") }
	dispatch.launch = func(index int) (bool, error) {
		slot, ok := slots.tryAcquire()
		if !ok {
			return false, nil
		}
		started <- slot
		go func() {
			select {
			case <-finish[index]:
			case <-ctx.Done():
			}
			results <- parallelBranchResult{index: index, status: journal.BranchSucceeded, slot: slot}
		}()
		return true, nil
	}
	go func() { done <- dispatch.run() }()
	nextSlot := func() *parallelBranchSlot {
		t.Helper()
		select {
		case slot := <-started:
			return slot
		case <-ctx.Done():
			t.Fatal("branch did not start", ctx.Err())
			return nil
		}
	}
	first := nextSlot()
	first.release()
	second := nextSlot()
	if first == second {
		t.Fatal("siblings share a slot owner")
	}
	rd, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventBranchFinished {
			t.Fatal("parking finished a branch")
		}
	}
	aborted, stop := context.WithCancel(ctx)
	stop()
	if err := first.acquire(aborted); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled branch reacquired execution", err)
	}
	resumed := make(chan error, 1)
	go func() { resumed <- first.acquire(ctx) }()
	close(finish[1])
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("continuation did not reacquire released slot")
	}
	close(finish[0])
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("parallel did not settle")
	}
	if len(slots.occupied) != 0 {
		t.Fatal("settled branches leaked execution slots")
	}
	events, err = rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var order []int
	for _, event := range events {
		if event.Type == journal.EventBranchFinished {
			order = append(order, event.Branch)
		}
	}
	if len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatal("waiting parent settled before running sibling", order)
	}
}
