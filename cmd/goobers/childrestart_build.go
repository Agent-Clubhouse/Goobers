package main

import (
	"context"
	"errors"
	"reflect"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

func (l *queuedChildLauncher) restartReference(ctx context.Context, plan runner.StageRestartPlan) (childExecutionRef, error) {
	ref, err := retainedChildExecutionRef(ctx, l.queue, plan.Source, false)
	if err != nil {
		return childExecutionRef{}, err
	}
	ref, err = childEpochReference(ctx, l.queue, ref, plan.Continuation.RunID)
	if err != nil {
		return childExecutionRef{}, err
	}
	e := ref.Execution
	if ref.Child.ActiveRunID() != e.RunID || ref.Child.CancellationRequested || ref.Child.State.Terminal() || e.SourceRunID != plan.Source.RunID || e.SourceTerminalSeq != plan.Continuation.ExpectedTerminalSeq || e.Actor != plan.Continuation.Operator || e.Stage != plan.Continuation.Target {
		return childExecutionRef{}, triggerqueue.ErrTransition
	}
	if err = l.queue.CheckChildParentOpen(ctx, ref.Child.Identity.ChildParent); err != nil {
		return childExecutionRef{}, err
	}
	return ref, nil
}

// Current child authority is held only while deriving the admitted runtime.
// Human policy is the caller's outer lease; the queue pins the immutable archive
// after this short constructor lease ends.
func (l *queuedChildLauncher) restartRuntime(ctx context.Context, ref childExecutionRef) (preparedChildRuntime, error) {
	if l.build == nil {
		return preparedChildRuntime{}, childworkflow.ErrAuthorityUnavailable
	}
	a, release, err := l.acquire(ctx, ref.Envelope)
	if err != nil {
		return preparedChildRuntime{}, err
	}
	defer release()
	source, err := l.queue.ChildProposal(ctx, ref.Child.Identity)
	if err != nil {
		return preparedChildRuntime{}, err
	}
	proposal, err := childworkflow.ValidateRetainedStart(a, ref.Envelope, source.Source)
	if err != nil {
		return preparedChildRuntime{}, err
	}
	return l.build(ctx, childExecutionStart{childExecutionRef: ref, Proposal: proposal})
}

func (l *queuedChildLauncher) buildRestartExecution(ctx context.Context, plan runner.StageRestartPlan) (intervention.Execution, error) {
	ref, err := l.restartReference(ctx, plan)
	if err != nil {
		return intervention.Execution{}, err
	}
	if !reflect.DeepEqual(plan.Continuation.ChildContinuation, &ref.Lineage) {
		return intervention.Execution{}, errors.New("child restart lineage differs from queue admission")
	}
	runtime, err := l.restartRuntime(ctx, ref)
	if err != nil {
		return intervention.Execution{}, err
	}
	if runtime.release != nil {
		defer runtime.release()
	}
	if runtime.machine.Digest() != plan.Source.WorkflowDigest || runtime.gooberDigest != plan.Source.GooberDigest {
		return intervention.Execution{}, errors.New("child restart definition differs from source")
	}
	return intervention.Execution{Runner: runtime.runner, Machine: runtime.machine, GooberDigest: runtime.gooberDigest, RepoRef: runtime.repoRef, ChildRestart: &intervention.ChildStageRestartAdmission{Entry: runtime.entry, Fence: func(ctx context.Context, callback func() error) error {
		return l.queue.WithChildExecutionResume(ctx, ref.Child.Identity, ref.runID(), callback)
	}}}, nil
}

func (s *interactiveStageRestart) preflightChildRestart(ctx context.Context, plan *runner.StageRestartPlan, load interactiveaccess.RestartSourceLoader) ([]localscheduler.ClaimEntry, error) {
	l := s.setup.ChildRestarts
	ref, err := l.restartReference(ctx, *plan)
	if err != nil {
		return nil, err
	}
	runtime, err := l.restartRuntime(ctx, ref)
	if err != nil {
		return nil, err
	}
	if runtime.release != nil {
		defer runtime.release()
	}
	required, err := authorizeChildRestartSources(ctx, runtime, load)
	if err != nil {
		return nil, err
	}
	plan.Continuation.ChildContinuation = &ref.Lineage
	var workspace *runner.ChildWorkspaceAdmission
	if required {
		url, err := childRepoCloneURL(runtime.repoRef)
		if err != nil {
			return nil, err
		}
		workspace, err = l.restartWorkspace(ctx, ref, runtime, url)
		if err != nil {
			return nil, err
		}
	}
	*plan, err = runner.BindChildRestartWorkspace(*plan, workspace)
	return nil, err // the waiting parent keeps its work-item/PR claims
}

func childRuntimeNeedsWorkspace(runtime preparedChildRuntime) bool {
	for _, task := range runtime.machine.Def.Spec.Tasks {
		if task.EffectiveWorkspace().IsRepoBacked() {
			return true
		}
	}
	for _, gate := range runtime.machine.Def.Spec.Gates {
		if gate.EffectiveWorkspace().IsRepoBacked() {
			return true
		}
	}
	return false
}

func (l *queuedChildLauncher) restartWorkspace(ctx context.Context, ref childExecutionRef, runtime preparedChildRuntime, url string) (*runner.ChildWorkspaceAdmission, error) {
	observed, err := acceptedChildObserver(l.layout)(ctx, ref)
	if err != nil {
		return nil, err
	}
	custody := childworkflow.WorkspaceCoordinator{Queue: l.queue, Worktrees: runtime.worktrees}
	if !observed {
		return custody.PrepareExecution(ctx, ref.Child, url)
	}
	dir, err := l.layout.FindRunDir(ref.runID())
	if err != nil {
		return nil, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return nil, err
	}
	id, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	admission, err := runner.PinnedChildWorkspaceAdmission(reader, id)
	if err != nil || admission == nil {
		return nil, errors.Join(err, errors.New("human child fork custody missing"))
	}
	fork, err := custody.ExecutionFork(ctx, ref.Child, ref.runID(), url)
	if err != nil {
		return nil, err
	}
	if admission.ForkSHA != fork.Record.SnapshotSHA || admission.RepositoryDigest != worktree.RepositoryDigest(url) {
		return nil, errors.New("human child fork custody changed")
	}
	if _, err = runtime.worktrees.AdoptChildFromSnapshot(ctx, worktree.ChildOptions{RepoURL: url, RunID: admission.WorkspaceID, OwnerRunID: ref.runID(), Gaggle: ref.Envelope.Gaggle, SnapshotSHA: admission.ForkSHA}); err != nil {
		return nil, err
	}
	return admission, nil
}

func (s *interactiveStageRestart) preflightNewChildRestart(ctx context.Context, ref childExecutionRef, load interactiveaccess.RestartSourceLoader) error {
	runtime, err := s.setup.ChildRestarts.restartRuntime(ctx, ref)
	if err != nil {
		return err
	}
	if runtime.release != nil {
		defer runtime.release()
	}
	_, err = authorizeChildRestartSources(ctx, runtime, load)
	return err
}

func authorizeChildRestartSources(ctx context.Context, runtime preparedChildRuntime, load interactiveaccess.RestartSourceLoader) (bool, error) {
	required := childRuntimeNeedsWorkspace(runtime)
	request := interactiveaccess.RestartSourceRequest{}
	if required {
		repo := interactiveRepositoryIdentity(runtime.repoRef)
		request.Repository = &repo
	}
	sources, err := load(ctx, request)
	if err != nil {
		return required, err
	}
	if sources.Gaggle.Spec.Enabled != nil && !*sources.Gaggle.Spec.Enabled {
		return required, interactiveaccess.ErrDenied
	}
	if required && !restartRepositoryConfigured(runtime.repoRef, sources.Gaggle) {
		return required, interactiveaccess.ErrDenied
	}
	return required, nil
}
