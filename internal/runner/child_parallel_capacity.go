package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type parallelChildBranchState uint8

const (
	parallelChildRunnable parallelChildBranchState = iota
	parallelChildParked
	parallelChildFinished
)

// One coordinator owns a whole parallel block. Declared but unstarted branches
// remain runnable: a single waiting branch cannot lend their run permit away.
// The mutex spans durable publication and scheduler changes. In particular,
// another branch cannot suspend between a successful reacquire and continued.
type parallelChildCapacity struct {
	mu         sync.Mutex
	runID      string
	capacity   ChildParentCapacity
	branches   map[int]parallelChildBranchState
	epochs     map[int]uint64
	suspension ChildParentSuspension
	broken     error
}

type parallelChildSuspension struct {
	owner  *parallelChildCapacity
	branch int
	epoch  uint64
}

func newParallelChildCapacity(runID string, capacity ChildParentCapacity, branches, parked, finished []int) (*parallelChildCapacity, error) {
	if runID == "" || capacity == nil || len(branches) == 0 || len(branches) > 128 {
		return nil, errors.New("runner: parallel child capacity requires bounded branch ownership")
	}
	p := &parallelChildCapacity{runID: runID, capacity: capacity, branches: make(map[int]parallelChildBranchState), epochs: make(map[int]uint64)}
	for _, branch := range branches {
		if _, exists := p.branches[branch]; exists || branch <= 0 {
			return nil, errors.New("runner: parallel child branch declarations are invalid")
		}
		p.branches[branch] = parallelChildRunnable
	}
	if err := p.restore(parked, parallelChildParked); err != nil {
		return nil, err
	}
	if err := p.restore(finished, parallelChildFinished); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *parallelChildCapacity) restore(branches []int, state parallelChildBranchState) error {
	for _, branch := range branches {
		current, exists := p.branches[branch]
		if !exists || current != parallelChildRunnable {
			return errors.New("runner: parallel child recovery has conflicting branch state")
		}
		p.branches[branch] = state
		if state == parallelChildParked {
			p.epochs[branch] = 1
		}
	}
	return nil
}

// Park publishes the exact wait and yields physical writer custody before the
// branch ceases to be runnable. Its caller releases the branch lane on success.
// The callback must not reenter this coordinator.
func (p *parallelChildCapacity) Park(ctx context.Context, branch int, publishWait func() error) (*parallelChildSuspension, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(ctx, branch); err != nil {
		return nil, err
	}
	if p.branches[branch] == parallelChildFinished {
		return nil, errors.New("runner: finished branch cannot wait for a child")
	}
	if p.branches[branch] != parallelChildParked {
		if err := p.publish(publishWait); err != nil {
			return nil, err
		}
		p.branches[branch] = parallelChildParked
		p.epochs[branch]++
	}
	if err := p.suspendIfParked(ctx); err != nil {
		return nil, err
	}
	return &parallelChildSuspension{owner: p, branch: branch, epoch: p.epochs[branch]}, nil
}

// Resume requires the caller to own a branch execution lane. Scheduler refusal
// is retryable without publishing continued or consuming a new start allowance.
func (p *parallelChildCapacity) Resume(ctx context.Context, branch int, publishContinued func() error) error {
	return p.resume(ctx, branch, 0, publishContinued)
}

func (s *parallelChildSuspension) Resume(ctx context.Context, publishContinued func() error) error {
	if s == nil || s.owner == nil {
		return errors.New("runner: parallel child suspension is missing")
	}
	return s.owner.resume(ctx, s.branch, s.epoch, publishContinued)
}

func (p *parallelChildCapacity) resume(ctx context.Context, branch int, epoch uint64, publishContinued func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(ctx, branch); err != nil {
		return err
	}
	if epoch != 0 && epoch != p.epochs[branch] {
		return errors.New("runner: parallel child suspension was superseded")
	}
	switch p.branches[branch] {
	case parallelChildRunnable:
		return nil
	case parallelChildFinished:
		return errors.New("runner: finished branch cannot resume")
	}
	// Recovery may reconstruct an entirely parked block before obtaining its
	// idempotent scheduler handle. This cannot dispatch or refund a start.
	if err := p.suspendIfParked(ctx); err != nil {
		return err
	}
	if p.suspension != nil {
		if err := p.suspension.Resume(ctx); err != nil {
			return err
		}
		p.suspension = nil
	}
	if err := p.publish(publishContinued); err != nil {
		return err
	}
	p.branches[branch] = parallelChildRunnable
	return nil
}

// Finish publishes branch settlement before checking whether the remaining
// branches all wait. Ending a live sibling can be the last transition needed to
// free the whole-run slot for an accepted child.
func (p *parallelChildCapacity) Finish(ctx context.Context, branch int, publishFinished func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(ctx, branch); err != nil {
		return err
	}
	if p.branches[branch] == parallelChildFinished {
		return nil
	}
	if err := p.publish(publishFinished); err != nil {
		return err
	}
	p.branches[branch] = parallelChildFinished
	return p.suspendIfParked(ctx)
}

func (p *parallelChildCapacity) check(ctx context.Context, branch int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.broken != nil {
		return p.broken
	}
	if _, exists := p.branches[branch]; !exists {
		return errors.New("runner: parallel child branch was not declared")
	}
	return nil
}

func (p *parallelChildCapacity) allRemainingParked() bool {
	waiting := false
	for _, state := range p.branches {
		if state == parallelChildRunnable {
			return false
		}
		waiting = waiting || state == parallelChildParked
	}
	return waiting
}

func (p *parallelChildCapacity) suspendIfParked(ctx context.Context) error {
	if p.suspension != nil || !p.allRemainingParked() {
		return nil
	}
	suspension, err := p.capacity.SuspendChildParent(ctx, p.runID)
	if err == nil && suspension == nil {
		err = errors.New("missing scheduler suspension")
	}
	if err != nil {
		p.broken = fmt.Errorf("runner: parallel child suspension requires recovery: %w", err)
		return p.broken
	}
	p.suspension = suspension
	return nil
}

func (p *parallelChildCapacity) publish(callback func() error) error {
	if callback == nil {
		p.broken = errors.New("runner: parallel child transition lacks durable publication")
	} else if err := callback(); err != nil {
		// A failed append may have reached disk. Stop this coordinator rather
		// than permit another branch to guess its effect on durable ownership.
		p.broken = fmt.Errorf("runner: parallel child publication requires recovery: %w", err)
	}
	return p.broken
}
