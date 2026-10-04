package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const childJournalHandoffTimeout = 30 * time.Second

type childPinnedAuthority interface {
	AcquirePinnedStage(context.Context, journal.RunIdentity, string) (childworkflow.PinnedStageAdmission, func(), error)
}

// queuedChildLauncher owns only admitted execution. Tool grants may have expired
// since acceptance: exact parent provenance plus applied current policy govern
// this handoff. Result custody is supplied separately by the durable disposition
// coordinator; absence never invents a terminal result or acknowledges a slot.
type queuedChildLauncher struct {
	layout    instance.Layout
	config    *instance.Config
	queue     *triggerqueue.Store
	authority childPinnedAuthority
	build     childRuntimeBuilder
	dispatch  *daemonTriggerService
	runners   *daemonRunnerRegistry
	wg        *sync.WaitGroup
	result    func(context.Context, childExecutionRef, *journal.Reader) (childExecutionResult, error)
	reconcile func(context.Context, *journal.Reader) error
}

func (l *queuedChildLauncher) Prepare(ctx context.Context, e childworkflow.ChildStartEnvelope) (childworkflow.Authority, error) {
	a, release, err := l.acquire(ctx, e)
	if err == nil {
		release()
	}
	return a, err
}

func (l *queuedChildLauncher) acquire(ctx context.Context, e childworkflow.ChildStartEnvelope) (childworkflow.Authority, func(), error) {
	if l.authority == nil {
		return childworkflow.Authority{}, nil, childworkflow.ErrAuthorityUnavailable
	}
	dir, err := l.layout.FindRunDir(e.ParentRunID)
	if err != nil {
		return childworkflow.Authority{}, nil, err
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		return childworkflow.Authority{}, nil, err
	}
	id, err := rd.Identity()
	if err != nil {
		return childworkflow.Authority{}, nil, err
	}
	if id.Child != nil || id.RunID != e.ParentRunID || id.Gaggle != e.Gaggle || id.Workflow != e.ParentWorkflow || id.WorkflowDigest != e.ParentWorkflowDigest || id.GooberDigest != e.ParentGooberDigest || id.ConfigGeneration != e.ConfigGeneration {
		return childworkflow.Authority{}, nil, childworkflow.ErrAuthorityUnavailable
	}
	pinned, release, err := l.authority.AcquirePinnedStage(ctx, id, e.ParentStage)
	if err != nil {
		return childworkflow.Authority{}, nil, err
	}
	validator, err := childworkflow.NewValidator(pinned.Admission)
	if err != nil {
		release()
		return childworkflow.Authority{}, nil, err
	}
	a := childworkflow.Authority{Origin: childworkflow.Origin{Gaggle: id.Gaggle, RunID: id.RunID, StageOccurrence: e.StageOccurrence, ConfigDigest: pinned.Admission.ConfigDigest, PolicyDigest: validator.PolicyDigest()},
		Actor: childworkflow.InvocationActor(id.RunID, e.StageOccurrence), Admission: pinned.Admission, ConfigGeneration: pinned.ConfigGeneration,
		ParentWorkflow: id.Workflow, ParentWorkflowDigest: pinned.WorkflowDigest, ParentGooberDigest: pinned.GooberDigest}
	return a, release, nil
}

func (l *queuedChildLauncher) Start(ctx context.Context, start childExecutionStart) error {
	if l.build == nil || l.dispatch == nil || l.queue == nil {
		return &childStartDeferred{Reason: "child execution runtime unavailable"}
	}
	if l.dispatch.schedulerReady != nil && !l.dispatch.schedulerReady() {
		return &childStartDeferred{Reason: "scheduler recovery incomplete"}
	}
	sched := l.dispatch.sched.Load()
	if sched == nil {
		return &childStartDeferred{Reason: "child scheduler unavailable"}
	}
	// Recheck exact current policy and source while holding the reload lease all
	// the way through publication. Proposal arguments are never trusted custody.
	a, releaseAuthority, err := l.acquire(ctx, start.Envelope)
	if err != nil {
		return err
	}
	defer releaseAuthority()
	source, err := l.queue.ChildProposal(ctx, start.Child.Identity)
	if err != nil {
		return err
	}
	start.Proposal, err = childworkflow.ValidateRetainedStart(a, start.Envelope, source.Source)
	if err != nil {
		return err
	}
	if observed, err := acceptedChildObserver(l.layout)(ctx, start.childExecutionRef); err != nil || observed {
		return errors.Join(err, errors.New("child journal already published; reconcile instead of starting"))
	}
	runtime, err := l.build(ctx, start)
	if err != nil {
		return err
	}
	defer func() {
		if runtime.release != nil {
			runtime.release()
		}
	}()
	workspace, err := l.prepareWorkspace(ctx, start, runtime)
	if err != nil {
		return err
	}
	releaseCapacity, err := sched.ReserveChild(ctx, localscheduler.ChildAdmissionRequest{RunID: start.Child.RunID, ParentRunID: start.Envelope.ParentRunID,
		Parent: localscheduler.WorkflowIdentity{Gaggle: start.Envelope.Gaggle, Workflow: start.Envelope.ParentWorkflow}, Child: runtime.entry}, time.Now())
	if err != nil {
		return &childStartDeferred{Reason: err.Error()}
	}
	// The execution goroutine inherits both leases, including on ambiguous
	// publication failure; Start returning cannot prematurely unpin its archive.
	releaseRuntime := runtime.release
	runtime.release = nil
	return l.handoff(ctx, start, runtime, workspace, releaseCapacity, releaseRuntime)
}

func (l *queuedChildLauncher) prepareWorkspace(ctx context.Context, start childExecutionStart, runtime preparedChildRuntime) (*runner.ChildWorkspaceAdmission, error) {
	if len(runtime.machine.Def.Spec.Parallels) > 0 {
		return nil, &childStartDeferred{Reason: "child parallel workspace admission unavailable"}
	}
	required := false
	for _, task := range runtime.machine.Def.Spec.Tasks {
		required = required || task.EffectiveWorkspace() != apiv1.WorkspaceScratch
	}
	for _, gate := range runtime.machine.Def.Spec.Gates {
		required = required || gate.EffectiveWorkspace() != apiv1.WorkspaceScratch
	}
	if !required {
		return nil, nil
	}
	url, err := childRepoCloneURL(runtime.repoRef)
	if err != nil {
		return nil, err
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: l.queue, Worktrees: runtime.worktrees}
	workspace, err := coordinator.Prepare(ctx, start.Child, url)
	if errors.Is(err, triggerqueue.ErrChildSnapshotPending) {
		return nil, &childStartDeferred{Reason: err.Error()}
	}
	return workspace, err
}

func (l *queuedChildLauncher) handoff(ctx context.Context, start childExecutionStart, runtime preparedChildRuntime, workspace *runner.ChildWorkspaceAdmission, releaseCapacity, releaseRuntime func()) error {
	published := make(chan struct{})
	permission := make(chan error, 1)
	done := make(chan error, 1)
	launchCtx, cancel := context.WithCancel(l.dispatch.lifecycleContext(ctx))
	barrierCtx, barrierCancel := context.WithTimeout(ctx, childJournalHandoffTimeout)
	defer barrierCancel()
	untrack := l.runners.Track(start.Child.RunID, start.Envelope.Workflow, runtime.runner)
	launched := false
	err := l.queue.WithChildLaunch(barrierCtx, start.Child.Identity, func() error {
		launched = true
		if l.wg != nil {
			l.wg.Add(1)
		}
		go func() {
			if l.wg != nil {
				defer l.wg.Done()
			}
			defer cancel()
			defer releaseCapacity()
			if releaseRuntime != nil {
				defer releaseRuntime()
			}
			defer untrack()
			ceiling := start.Proposal.CredentialCeiling()
			_, runErr := runtime.runner.Start(launchCtx, runner.StartInput{RunID: start.Child.RunID, Machine: runtime.machine, GooberDigest: runtime.gooberDigest,
				Gaggle: start.Envelope.Gaggle, Child: &start.Lineage, ChildWorkspace: workspace, ChildCredentials: &ceiling, RepoRef: runtime.repoRef, RunControls: runtime.controls,
				RequiredCapabilities: runtime.entry.RequiredCapabilities, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: start.Child.AcceptanceID},
				OnJournalPublished: func() error { close(published); return <-permission },
			})
			done <- runErr
		}()
		select {
		case <-published:
			return nil
		case runErr := <-done:
			return fmt.Errorf("child runner returned before publication: %w", runErr)
		case <-barrierCtx.Done():
			return barrierCtx.Err()
		}
	})
	permission <- err
	if err != nil {
		cancel()
	}
	if !launched {
		untrack()
		cancel()
		releaseCapacity()
		if releaseRuntime != nil {
			releaseRuntime()
		}
		if errors.Is(err, triggerqueue.ErrParentCancelled) || errors.Is(err, triggerqueue.ErrParentSettled) {
			return &childStartDeferred{Reason: err.Error()}
		}
	}
	return err
}

func (l *queuedChildLauncher) Cancel(ctx context.Context, ref childExecutionRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if observed, err := acceptedChildObserver(l.layout)(ctx, ref); err != nil || !observed {
		return err
	}
	owner, ok := l.runners.Resolve(ref.Child.RunID, ref.Envelope.Gaggle, nil)
	if !ok {
		return nil
	} // restart recovery, not delivery, owns an absent runner
	_, _, err := owner.CancelRun(ref.Child.RunID, time.Now())
	return err
}

func (l *queuedChildLauncher) Result(ctx context.Context, ref childExecutionRef) (childExecutionResult, error) {
	if l.result == nil {
		return childExecutionResult{}, nil
	}
	observed, err := acceptedChildObserver(l.layout)(ctx, ref)
	if err != nil {
		return childExecutionResult{}, err
	}
	release, available := l.runners.acquireChildCustody(ref.Child.RunID)
	if !available {
		return childExecutionResult{}, nil
	}
	defer release()
	if !observed {
		return l.rejectedResult(ctx, ref)
	}
	dir, err := l.layout.FindRunDir(ref.Child.RunID)
	if err != nil {
		return childExecutionResult{}, err
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		return childExecutionResult{}, err
	}
	if l.reconcile != nil {
		if err := l.reconcile(ctx, rd); err != nil {
			return childExecutionResult{}, err
		}
		events, err := rd.Events()
		if err != nil {
			return childExecutionResult{}, err
		}
		if journal.PhaseFromEvents(events) == journal.PhaseRunning && !journal.ParkedAtGate(events) {
			return childExecutionResult{}, l.resumeOwnedChild(ctx, ref)
		}
	}
	return l.result(ctx, ref, rd)
}
