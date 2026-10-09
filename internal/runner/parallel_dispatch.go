package runner

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// parallelDispatch owns branch scheduling on the root journal goroutine.
// Execution and durable settlement remain callbacks of the existing runner;
// branch workers never mutate the queue, outcomes, or cancellation policy.
type parallelDispatch struct {
	queue                                 []int
	next, running                         int
	outcomes                              []*parallelBranchResult
	results                               <-chan parallelBranchResult
	released                              <-chan struct{}
	launch                                func(int) (bool, error)
	settle                                func(parallelBranchResult) error
	cancelQueued                          func() error
	cancel                                context.CancelCauseFunc
	failurePolicy                         apiv1.BranchFailurePolicy
	firstErr                              error
	terminalTriggered, failFast, draining bool
}

func (p *parallelDispatch) run() error {
	if p.terminalTriggered {
		if err := p.cancelQueued(); err != nil {
			return err
		}
	}
	p.launchAvailable()
	for p.running > 0 {
		select {
		case result := <-p.results:
			p.accept(result)
		case <-p.released:
		}
		p.launchAvailable()
	}
	return p.firstErr
}

func (p *parallelDispatch) launchAvailable() {
	for p.firstErr == nil && !p.terminalTriggered && !p.failFast && !p.draining && p.next < len(p.queue) {
		started, err := p.launch(p.queue[p.next])
		if err != nil {
			p.rememberFailure(err)
			return
		}
		if !started {
			return
		}
		p.running++
		p.next++
	}
}

func (p *parallelDispatch) rememberFailure(err error) {
	if err != nil && p.firstErr == nil {
		p.firstErr = err
		p.cancel(err)
	}
}

func (p *parallelDispatch) accept(result parallelBranchResult) {
	defer result.slot.release()
	p.running--
	p.outcomes[result.index] = &result
	if result.paused {
		p.draining = true
	} else {
		p.rememberFailure(p.settle(result))
	}
	p.rememberFailure(result.err)
	if result.terminalTarget != "" && !p.terminalTriggered {
		p.terminalTriggered = true
		p.cancel(errParallelTerminal)
	}
	if (result.status == journal.BranchFailed || result.status == journal.BranchTimedOut) && p.failurePolicy == apiv1.BranchFailFast && !p.failFast {
		p.failFast = true
		p.cancel(errParallelFailFast)
	}
	if (p.firstErr != nil || p.terminalTriggered || p.failFast) && p.next < len(p.queue) {
		if err := p.cancelQueued(); err != nil && p.firstErr == nil {
			p.firstErr = err
		}
	}
}

func settleConcurrentBranch(jr *journal.Run, par *parallelExec, name string, result parallelBranchResult) error {
	branch := par.branchSnapshot(result.index)
	cursors := par.settleBranch(branch.id, result.status, result.artifacts, result.pointers, result.produced, result.failed, result.noOutput)
	jr.SetBranchCursors(cursors)
	return jr.Append(journal.Event{Type: journal.EventBranchFinished, Branch: branch.id, Parallel: name, BranchName: branch.name, BranchStatus: result.status})
}
