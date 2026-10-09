package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
)

// resumeOwnedChild is called by the queue drain with exclusive registry custody
// after every previous physical worker has stopped and returned its artifacts.
// Refusal leaves the accepted execution pending for a later bounded drain.
func (l *queuedChildLauncher) resumeOwnedChild(ctx context.Context, ref childExecutionRef) error {
	if l.build == nil || l.dispatch == nil || l.runners == nil {
		return childworkflow.ErrAuthorityUnavailable
	}
	scheduler := l.dispatch.sched.Load()
	if scheduler == nil {
		return childworkflow.ErrAuthorityUnavailable
	}
	authority, release, err := l.acquire(ctx, ref.Envelope)
	if err != nil {
		return err
	}
	defer release()
	source, err := l.queue.ChildProposal(ctx, ref.Child.Identity)
	if err != nil {
		return err
	}
	proposal, err := childworkflow.ValidateRetainedStart(authority, ref.Envelope, source.Source)
	if err != nil {
		return err
	}
	runtime, err := l.build(ctx, childExecutionStart{childExecutionRef: ref, Proposal: proposal})
	if err != nil {
		return err
	}
	handed := false
	defer func() {
		if !handed && runtime.release != nil {
			runtime.release()
		}
	}()
	// Registry custody excludes a live driver. Replace any startup-restored
	// reservation with ordinary child capacity admission before resuming.
	scheduler.ReleaseReconciled(ref.Child.RunID, ref.Envelope.Workflow)
	capacity, err := scheduler.ReserveChild(ctx, localscheduler.ChildAdmissionRequest{RunID: ref.Child.RunID, ParentRunID: ref.Envelope.ParentRunID, Parent: localscheduler.WorkflowIdentity{Gaggle: ref.Envelope.Gaggle, Workflow: ref.Envelope.ParentWorkflow}, Child: runtime.entry}, time.Now())
	if err != nil {
		return &childStartDeferred{Reason: err.Error()}
	}
	defer func() {
		if !handed {
			capacity()
		}
	}()
	ready, permission := make(chan struct{}), make(chan error, 1)
	done := make(chan error, 1)
	launchCtx, cancel := context.WithCancel(l.dispatch.lifecycleContext(context.WithoutCancel(ctx)))
	barrierCtx, barrierCancel := context.WithTimeout(ctx, childJournalHandoffTimeout)
	defer barrierCancel()
	err = l.queue.WithChildResume(barrierCtx, ref.Child.Identity, ref.Child.RunID, func() error {
		untrack, ok := l.runners.promoteChildCustody(ref.Child.RunID, ref.Envelope.Workflow, runtime.runner)
		if !ok {
			return errors.New("child recovery ownership changed")
		}
		handed = true
		if l.wg != nil {
			l.wg.Add(1)
		}
		go func() {
			if l.wg != nil {
				defer l.wg.Done()
			}
			defer cancel()
			defer untrack()
			defer capacity()
			if runtime.release != nil {
				defer runtime.release()
			}
			_, runErr := runtime.runner.Resume(launchCtx, runner.ResumeInput{RunID: ref.Child.RunID, Machine: runtime.machine, GooberDigest: runtime.gooberDigest, RepoRef: runtime.repoRef, RecoveryReason: "isolated-child-custody-reconciled", OnRecoveryOwned: func(owned context.Context) error {
				close(ready)
				select {
				case err := <-permission:
					return err
				case <-owned.Done():
					return owned.Err()
				}
			}})
			done <- runErr
		}()
		select {
		case <-ready:
			return nil
		case runErr := <-done:
			return errors.Join(errors.New("child recovery returned before ownership"), runErr)
		case <-barrierCtx.Done():
			return barrierCtx.Err()
		}
	})
	permission <- err
	if err != nil || !handed {
		cancel()
	}
	return err
}

// promoteChildCustody publishes the cancellable execution owner atomically with
// exclusive recovery custody, leaving no gap for a competing drain or tracker.
func (r *daemonRunnerRegistry) promoteChildCustody(runID, workflow string, owner *runner.Runner) (func(), bool) {
	if r == nil || owner == nil {
		return func() {}, false
	}
	r.mu.Lock()
	if r.childCustody[runID] == nil || r.owners[runID].owner != nil || r.hardStopping {
		r.mu.Unlock()
		return func() {}, false
	}
	if r.owners == nil {
		r.owners = map[string]trackedRun{}
	}
	r.nextGeneration++
	generation := r.nextGeneration
	r.owners[runID] = trackedRun{RunID: runID, Workflow: workflow, owner: owner, generation: generation, leases: 1}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			lease := r.owners[runID]
			if lease.generation == generation {
				lease.leases--
				if lease.leases == 0 {
					delete(r.owners, runID)
				} else {
					r.owners[runID] = lease
				}
			}
		})
	}, true
}
