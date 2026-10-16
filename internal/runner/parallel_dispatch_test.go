package runner

import (
	"context"
	"errors"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestParallelDispatchDrainsWritersBeforeReturning(t *testing.T) {
	journalFailure := errors.New("settlement journal failed")
	workerFailure := errors.New("worker failed")
	for _, tc := range []struct {
		name       string
		first      parallelBranchResult
		settleErr  error
		wantErr    error
		wantCause  error
		wantPaused bool
	}{
		{name: "journal failure wins", first: parallelBranchResult{err: workerFailure}, settleErr: journalFailure, wantErr: journalFailure, wantCause: journalFailure},
		{name: "worker failure", first: parallelBranchResult{err: workerFailure}, wantErr: workerFailure, wantCause: workerFailure},
		{name: "pause preserves queued work", first: parallelBranchResult{paused: true}, wantPaused: true},
		{name: "uncertain custody preserves queued work", first: parallelBranchResult{paused: true, status: journal.BranchCancelled, err: invoke.ErrChildCustodyPending}, wantErr: invoke.ErrChildCustodyPending, wantCause: invoke.ErrChildCustodyPending, wantPaused: true},
		{name: "fail fast", first: parallelBranchResult{status: journal.BranchFailed}, wantCause: errParallelFailFast},
		{name: "terminal", first: parallelBranchResult{terminalTarget: "blocked"}, wantCause: errParallelTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			results := make(chan parallelBranchResult, 2)
			slots := newParallelBranchSlots(2)
			launched, settled := []int{}, []int{}
			p := &parallelDispatch{queue: []int{0, 1, 2}, released: slots.changed, outcomes: make([]*parallelBranchResult, 3), results: results, cancel: cancel, failurePolicy: apiv1.BranchFailFast}
			p.launch = func(index int) (bool, error) {
				slot, available := slots.tryAcquire()
				if !available {
					return false, nil
				}
				launched = append(launched, index)
				result := parallelBranchResult{index: index, status: journal.BranchSucceeded}
				if index == 0 {
					result = tc.first
				}
				result.slot = slot
				results <- result
				return true, nil
			}
			p.settle = func(result parallelBranchResult) error {
				settled = append(settled, result.index)
				if result.index == 0 {
					return tc.settleErr
				}
				return nil
			}
			cancelled := 0
			p.cancelQueued = func() error {
				cancelled++
				p.next = len(p.queue)
				return nil
			}
			if err := p.run(); !errors.Is(err, tc.wantErr) || !errors.Is(context.Cause(ctx), tc.wantCause) {
				t.Fatal("dispatch changed failure precedence", err, context.Cause(ctx))
			}
			if !slices.Equal(launched, []int{0, 1}) || p.running != 0 || p.outcomes[1] == nil {
				t.Fatal("dispatch launched queued work or abandoned a running writer", launched, p.running)
			}
			wantSettled, wantCancelled := []int{0, 1}, 1
			if tc.wantPaused {
				wantSettled, wantCancelled = []int{1}, 0
			}
			if !slices.Equal(settled, wantSettled) || cancelled != wantCancelled || p.draining != tc.wantPaused {
				t.Fatal("dispatch changed pause/settlement custody", settled, cancelled, p.draining)
			}
		})
	}
}

func TestParallelDispatchRetainedTerminalNeverLaunches(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	p := &parallelDispatch{queue: []int{0}, terminalTriggered: true, cancel: cancel}
	p.launch = func(int) (bool, error) {
		t.Fatal("recovered terminal launched another writer")
		return false, nil
	}
	p.cancelQueued = func() error {
		p.next = len(p.queue)
		return nil
	}
	if err := p.run(); err != nil || p.next != 1 || context.Cause(ctx) != nil {
		t.Fatal("retained terminal reconciliation changed", err, p.next)
	}
}

func TestParallelInvocationRetainsCustodyErrorDuringDrain(t *testing.T) {
	var result parallelBranchResult
	err := errors.Join(errChildWaitDrain, invoke.ErrChildCustodyPending)
	if !parallelInvocationFailed(err, &result) || !result.paused || !errors.Is(result.err, invoke.ErrChildCustodyPending) {
		t.Fatal("drain hid uncertain writer custody", result)
	}
}
