package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

const childResultArtifactBytes = 1 << 20

// captureTerminal runs only with registry custody after all execution owners
// have joined. It reads source-owned journal evidence and the admitted fork;
// current tool permissions cannot block preservation of already-produced work.
func (l *queuedChildLauncher) captureTerminal(ctx context.Context, ref childExecutionRef, rd *journal.Reader) (childExecutionResult, error) {
	events, err := rd.Events()
	if err != nil {
		return childExecutionResult{}, err
	}
	phase := journal.PhaseFromEvents(events)
	if phase == journal.PhaseEscalated || (phase == journal.PhaseRunning && journal.ParkedAtGate(events)) {
		return childExecutionResult{State: triggerqueue.ChildAwaitingHuman}, nil
	}
	input, found, err := childTerminalInput(rd, events, phase)
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

func childTerminalInput(rd *journal.Reader, events []journal.Event, phase journal.RunPhase) (childworkflow.TerminalResultInput, bool, error) {
	var state triggerqueue.ChildState
	switch phase {
	case journal.PhaseCompleted:
		state = triggerqueue.ChildCompleted
	case journal.PhaseFailed:
		state = triggerqueue.ChildFailed
	case journal.PhaseEscalated:
		state = triggerqueue.ChildAwaitingHuman
	case journal.PhaseAborted:
		state = triggerqueue.ChildCancelled
	default:
		return childworkflow.TerminalResultInput{}, false, nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != journal.EventRunFinished {
			continue
		}
		if events[i].Status != string(phase) || events[i].Time.IsZero() {
			return childworkflow.TerminalResultInput{}, false, errors.New("child terminal event disagrees with phase")
		}
		input := childworkflow.TerminalResultInput{State: state, FinishedAt: events[i].Time, Summary: "Child workflow " + string(phase)}
		refs, summary, err := childResultEvidence(rd, events[:i+1])
		if err != nil {
			return childworkflow.TerminalResultInput{}, false, err
		}
		input.References = refs
		if summary != "" {
			input.Summary = summary
		}
		return input, true, nil
	}
	return childworkflow.TerminalResultInput{}, false, nil
}

func childResultEvidence(rd *journal.Reader, events []journal.Event) ([]string, string, error) {
	seen := map[string]bool{}
	var refs []string
	var summary string
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if summary == "" && event.Type == journal.EventStageFinished {
			if text, ok := event.Outputs["summary"].(string); ok && utf8.ValidString(text) {
				summary = boundedChildSummary(text)
			}
		}
		for _, ref := range event.Artifacts {
			if len(refs) == 16 || seen[ref.Digest] || ref.Size > childResultArtifactBytes {
				continue
			}
			if _, err := rd.ArtifactBytesBounded(ref, childResultArtifactBytes); err != nil {
				return nil, "", err
			}
			seen[ref.Digest] = true
			refs = append(refs, ref.Digest)
		}
	}
	return refs, summary, nil
}

func boundedChildSummary(text string) string {
	if len(text) <= 8192 {
		return text
	}
	text = text[:8192]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
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
	fork, err := coordinator.ExecutionFork(ctx, ref.Child, id.RunID, url)
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
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		return nil, err
	}
	for i := range set.Gaggles {
		gaggle := &set.Gaggles[i]
		if gaggle.Name != id.Gaggle {
			continue
		}
		if id.WorkspaceRepository == nil || !reflect.DeepEqual(gaggle.Spec.Project, *id.WorkspaceRepository) {
			return nil, errors.New("child terminal repository differs from archive")
		}
		if configured, ok := configuredRepoForProject(l.config, gaggle.Spec.Project); ok && configured.Pinned() {
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
