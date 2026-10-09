package runner

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

// The block owner is reconstructed from the same journal projection used by
// scheduler recovery. Declared, unstarted siblings retain their run capacity.
func (r *Runner) parallelChildOwner(in StartInput, par *parallelExec, events []journal.Event) (*parallelChildCapacity, error) {
	if !hasContainedParentStage(in) {
		return nil, nil
	}
	projection, err := journal.ProjectChildWaits(events)
	if err != nil {
		return nil, err
	}
	if len(projection.Waits) != 0 && projection.Parallel != par.spec.Name {
		return nil, fmt.Errorf("runner: child wait belongs to a different parallel block")
	}
	var branches, parked, finished []int
	for i := range par.spec.Branches {
		branch := par.branchSnapshot(i)
		branches = append(branches, branch.id)
		if branch.settled {
			finished = append(finished, branch.id)
		}
		if wait, ok := projection.Waits[branch.id]; ok {
			if wait.Header.ParentRunID != in.RunID || wait.Header.Request.Gaggle != in.Gaggle {
				return nil, fmt.Errorf("runner: parallel child wait belongs to a different run")
			}
			parked = append(parked, branch.id)
		}
	}
	if len(parked) != len(projection.Waits) {
		return nil, fmt.Errorf("runner: parallel child wait has an undeclared branch")
	}
	return newParallelChildCapacity(in.RunID, r.cfg.ChildParentCapacity, branches, parked, finished)
}

func finishParallelChildBranch(ctx context.Context, owner *parallelChildCapacity, branch int, publish func() error) error {
	if owner == nil {
		return publish()
	}
	// Cancellation does not waive root settlement. This callback runs only
	// after the branch worker returned, or for a branch never launched.
	return owner.Finish(context.WithoutCancel(ctx), branch, publish)
}

type childBranchSuspension struct {
	serial   ChildParentSuspension
	parallel *parallelChildSuspension
	slot     *parallelBranchSlot
}

func (r *Runner) suspendChildBranch(ctx context.Context, tf *taskFrame, request ChildHandoffRequest) (*childBranchSuspension, error) {
	if tf.in.parallelChild == nil {
		suspension, err := r.cfg.ChildParentCapacity.SuspendChildParent(ctx, tf.in.RunID)
		return &childBranchSuspension{serial: suspension}, err
	}
	if tf.in.parallelSlot == nil {
		return nil, fmt.Errorf("runner: child wait lacks its parallel execution slot")
	}
	_, branch, err := OwnedJournalScope(tf.jr)
	if err != nil {
		return nil, err
	}
	if branch == 0 {
		return nil, fmt.Errorf("runner: child wait lacks its owned branch journal")
	}
	// The invocation owner already published the wait before yielding writer
	// custody. Verify that exact durable receipt, including repeated recovery
	// reconciliation, before permitting either execution capacity release.
	if err := verifyParallelChildWait(tf, request, branch); err != nil {
		return nil, err
	}
	suspension, err := tf.in.parallelChild.Park(ctx, branch, func() error {
		return verifyParallelChildWait(tf, request, branch)
	})
	if err != nil {
		return nil, err
	}
	tf.in.parallelSlot.release()
	return &childBranchSuspension{parallel: suspension, slot: tf.in.parallelSlot}, nil
}

func verifyParallelChildWait(tf *taskFrame, request ChildHandoffRequest, branch int) error {
	reader, err := journal.OpenReadOnly(tf.jr.Dir())
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	projection, err := journal.ProjectChildWaits(events)
	if err != nil {
		return err
	}
	wait, ok := projection.Waits[branch]
	if !ok || wait.Header.Request != journal.ChildHandoffRequest(request) || wait.Header.ParentRunID != tf.in.RunID || wait.Marker.Stage != tf.t.Name {
		return fmt.Errorf("runner: child capacity release lacks its exact durable branch wait")
	}
	return nil
}

type childBranchContinuation struct {
	suspension *childBranchSuspension
	continued  func() error
}

func (c *childBranchContinuation) Resume(ctx context.Context) error {
	if c.suspension == nil || c.continued == nil {
		return fmt.Errorf("runner: child continuation lacks capacity or publication")
	}
	s := c.suspension
	if s.parallel == nil {
		if s.serial == nil {
			return fmt.Errorf("runner: child capacity suspension is missing")
		}
		if err := s.serial.Resume(ctx); err != nil {
			return err
		}
		return c.continued()
	}
	if err := s.slot.acquire(ctx); err != nil {
		return err
	}
	if err := s.parallel.Resume(ctx, c.continued); err != nil {
		s.slot.release()
		return err
	}
	return nil
}
