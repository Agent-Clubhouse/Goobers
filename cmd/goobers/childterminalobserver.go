package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

// captureTerminal runs only with registry custody after all execution owners
// have joined. It reads source-owned journal evidence and the admitted fork;
// current tool permissions cannot block preservation of already-produced work.
func (l *queuedChildLauncher) captureTerminal(ctx context.Context, ref childExecutionRef, rd *journal.Reader) (childExecutionResult, error) {
	events, err := rd.Events()
	if err != nil {
		return childExecutionResult{}, err
	}
	phase := journal.PhaseFromEvents(events)
	if phase == journal.PhaseRunning && journal.ParkedAtGate(events) {
		return childExecutionResult{State: triggerqueue.ChildAwaitingHuman}, nil
	}
	input, found, err := childworkflow.TerminalInputFromJournal(rd, events, phase)
	if phase == journal.PhaseEscalated {
		input, found, err = l.cancelledEscalationInput(ctx, ref, rd, events)
		if err == nil && !found {
			return childExecutionResult{State: triggerqueue.ChildAwaitingHuman}, nil
		}
	}
	if err != nil || !found {
		return childExecutionResult{}, err
	}
	id, err := rd.Identity()
	if err != nil {
		return childExecutionResult{}, err
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: l.queue}
	workspace, err := l.terminalWorkspace(ctx, &coordinator, ref, rd, id)
	if err != nil {
		return childExecutionResult{}, err
	}
	result, err := coordinator.CaptureResult(ctx, ref.Child, workspace, input)
	if err != nil {
		return childExecutionResult{}, err
	}
	return childExecutionResult{State: result.Input.State, ResultRef: result.ResultRef, WorkspaceRef: result.WorkspaceRef}, nil
}

func (l *queuedChildLauncher) terminalWorkspace(ctx context.Context, coordinator *childworkflow.WorkspaceCoordinator, ref childExecutionRef, rd *journal.Reader, id journal.RunIdentity) (*childworkflow.YieldedWorkspace, error) {
	admission, err := runner.PinnedChildWorkspaceAdmission(rd, id)
	if err != nil {
		return nil, err
	}
	if admission == nil {
		return nil, nil
	}
	events, err := rd.Events()
	if err != nil {
		return nil, err
	}
	if err := runner.VerifyChildWorkspaceQuiescence(rd, id, events); err != nil {
		return nil, err
	}
	if id.WorkspaceRepository == nil {
		return nil, childworkflow.ErrAuthorityUnavailable
	}
	url, err := childRepoCloneURL(*id.WorkspaceRepository)
	if err != nil {
		return nil, err
	}
	fork, err := coordinator.RetainedFork(ctx, ref.Child, url)
	if err != nil {
		return nil, err
	}
	if admission.ForkSHA != fork.Record.SnapshotSHA || admission.RepositoryDigest != worktree.RepositoryDigest(url) {
		return nil, childworkflow.ErrAuthorityUnavailable
	}
	manager, err := l.retainedChildWorktrees(ctx, id)
	if err != nil {
		return nil, err
	}
	adopted, err := manager.AdoptChildFromSnapshot(ctx, worktree.ChildOptions{RepoURL: url, RunID: admission.WorkspaceID, OwnerRunID: id.RunID, Gaggle: id.Gaggle, SnapshotSHA: admission.ForkSHA})
	if err != nil {
		return nil, err
	}
	return &childworkflow.YieldedWorkspace{Path: adopted.Path, RepoURL: url, RepositoryKey: fork.Record.RepositoryKey, Policy: fork.Policy}, nil
}

func (l *queuedChildLauncher) retainedChildWorktrees(ctx context.Context, id journal.RunIdentity) (*worktree.Manager, error) {
	if l.config == nil {
		return nil, childworkflow.ErrAuthorityUnavailable
	}
	store, err := executionGenerationStore(l.layout)
	if err != nil {
		return nil, err
	}
	directory, lease, err := store.Acquire(ctx, id.ConfigGeneration)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Release() }()
	set, report, err := loadConfigDirectory(directory)
	if err != nil {
		return nil, fmt.Errorf("load child terminal generation: %w (%s)", err, runtimewiring.ValidationIssueSummary(report))
	}
	for i := range set.Gaggles {
		gaggle := &set.Gaggles[i]
		if gaggle.Name != id.Gaggle {
			continue
		}
		if id.WorkspaceRepository == nil || !reflect.DeepEqual(gaggle.Spec.Project, *id.WorkspaceRepository) {
			return nil, errors.New("child terminal repository differs from archive")
		}
		if configured, ok := runtimewiring.ConfiguredRepoForProject(l.config, gaggle.Spec.Project); ok && configured.Pinned() {
			return nil, errors.New("child terminal custody cannot use shared pinned workspace")
		}
		scoped, err := instance.EffectiveWorkcopiesLayout(l.layout.ForGaggle(id.Gaggle), l.config, gaggle)
		if err != nil {
			return nil, err
		}
		return worktree.NewManager(scoped.WorkcopiesDir())
	}
	return nil, fmt.Errorf("child gaggle %q missing from retained archive", id.Gaggle)
}

func (l *queuedChildLauncher) rejectedResult(ctx context.Context, ref childExecutionRef) (childExecutionResult, error) {
	at, _, err := l.queue.ChildRejection(ctx, ref.Child.Identity)
	if errors.Is(err, sql.ErrNoRows) {
		return childExecutionResult{}, nil
	}
	if err != nil {
		return childExecutionResult{}, err
	}
	state := triggerqueue.ChildFailed
	if ref.Child.CancellationRequested {
		state = triggerqueue.ChildCancelled
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: l.queue}
	result, err := coordinator.CaptureResult(ctx, ref.Child, nil, childworkflow.TerminalResultInput{State: state, FinishedAt: at, Summary: "Child start rejected before execution"})
	if err != nil {
		return childExecutionResult{}, err
	}
	return childExecutionResult{State: result.Input.State, ResultRef: result.ResultRef}, nil
}
