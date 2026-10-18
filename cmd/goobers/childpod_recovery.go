package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// Each ordinary drain performs at most one bounded worker observation per child.
// The workflow itself retains its longer stop/join deadline across these polls.
const childCustodyObservationTimeout = 5 * time.Second

func (s *daemonCredentialService) recoverChildPod(ctx context.Context, reader *journal.Reader, digest string, scope childPodScope, client childpod.TemporalClient, surrenders dispatcher.SurrenderPlane) error {
	observation, cancel := context.WithTimeout(ctx, childCustodyObservationTimeout)
	report, err := (childpod.TemporalDispatch{Client: client}).Reconcile(observation, scope.Retained.Input)
	cancel()
	if err != nil {
		return err
	}
	if !report.ChildCreateAttempted || !report.WorkspaceWritersStopped || !report.SurrenderConfirmed || report.ChildPodUID == "" || report.Local {
		return invoke.ErrWorkspaceNotQuiescent
	}
	// The caller owns the registry's exclusive child-custody reservation. The
	// nonblocking journal lock also excludes a remote/live writer before import.
	writer, _, err := journal.TryRecover(reader.Dir(), journal.WithScrubber(s.shared))
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()
	pending, events, err := s.pendingChildPodScopes(ctx, reader)
	if err != nil {
		return err
	}
	current, ok := pending[digest]
	if !ok || current.Event.Seq != scope.Event.Seq {
		return invoke.ErrWorkspaceNotQuiescent
	}
	owned, stop := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer stop()
	request, blobs, err := s.childRecoveryRequest(owned, reader, scope)
	if err != nil {
		return err
	}
	recorder, err := runner.OwnedBranchRecorder(writer, scope.Contract.ChildBranch)
	if err != nil {
		return err
	}
	executor := childpod.Executor{Blobs: blobs, Surrenders: surrenders, Recorder: recorder, RecoveryReader: reader}
	out, err := executor.Reconcile(owned, request, scope.Retained, report)
	if err != nil {
		return err
	}
	attemptBlobs := childpod.ChildAttemptBlobs{Store: blobs, ContractDigest: digest}
	if err = adoptContainedPodOutputs(owned, recorder, attemptBlobs, &out); err != nil {
		return err
	}
	if request.Workspace != nil {
		var origin journal.Event
		for _, event := range events {
			if event.Seq == uint64(scope.Contract.PodAttempt) {
				origin = event
				break
			}
		}
		if err = runner.ReconcileChildWorkspaceWriter(reader, writer, scope.Contract.Identity, origin, scope.Event); err != nil {
			return err
		}
	}
	// Preserve the surrendered observation (including actual usage and pointers)
	// without fabricating a stage completion. Ordinary Resume still applies its
	// interrupted-attempt and missing-budget rules to unfinished stage journals.
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	recovered, err := recorder.RecordArtifactBoundedWithIntegrity("contained-recovery/"+digest[7:], data, apiv1.IntegrityDerived, childpod.MaxRetainedAttemptBytes)
	if err != nil {
		return err
	}
	return writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Artifacts: []journal.Ref{recovered}, Stage: scope.Event.Stage, Attempt: scope.Event.Attempt, Branch: scope.Event.Branch,
		Runner: map[string]any{"kind": childPodWriterJoined, "contractDigest": digest, "retainedAttempt": scope.Event.Runner["retainedAttempt"]}})
}

func (s *daemonCredentialService) childRecoveryRequest(ctx context.Context, reader *journal.Reader, scope childPodScope) (childpod.Request, childpod.ScopedBlobs, error) {
	c := scope.Contract
	request := childpod.Request{ChildBranch: c.ChildBranch, Identity: c.Identity, Attempt: scope.Retained.Input.Attempt, Eligible: scope.Retained.Input.Eligible, Ceiling: c.Ceiling, StartedAt: c.StartedAt}
	child, err := s.childQueue.ChildForExecutionRun(ctx, c.Identity.RunID)
	if err != nil {
		return request, childpod.ScopedBlobs{}, err
	}
	blobs := childpod.ScopedBlobs{Queue: s.childQueue, Identity: child.Identity}
	if c.Workspace == nil {
		return request, blobs, nil
	}
	admission, err := runner.PinnedChildWorkspaceAdmission(reader, c.Identity)
	if err != nil {
		return request, blobs, err
	}
	if admission == nil || c.Identity.WorkspaceRepository == nil {
		return request, blobs, errors.New("child recovery workspace admission missing")
	}
	url, err := childRepoCloneURL(*c.Identity.WorkspaceRepository)
	if err != nil {
		return request, blobs, err
	}
	fork, err := (&childworkflow.WorkspaceCoordinator{Queue: s.childQueue}).RetainedFork(ctx, child, url)
	if err != nil {
		return request, blobs, err
	}
	if fork.Record.SnapshotSHA != admission.ForkSHA || fork.Record.RepositoryKey != childRepoKey(*c.Identity.WorkspaceRepository) || admission.RepositoryDigest != worktree.RepositoryDigest(url) {
		return request, blobs, errors.New("child recovery fork differs from retained admission")
	}
	// Custody restoration uses immutable archive placement, never current tool
	// authority. It does not reset, create, fetch, or recapture a workspace.
	launcher := &queuedChildLauncher{layout: s.layout, config: s.config}
	manager, err := launcher.retainedChildWorktrees(ctx, c.Identity)
	if err != nil {
		return request, blobs, err
	}
	workspace, err := adoptChildStageWorkspace(ctx, manager, worktree.ChildOptions{RepoURL: url, RunID: admission.WorkspaceID, OwnerRunID: c.Identity.RunID, Gaggle: c.Identity.Gaggle, SnapshotSHA: admission.ForkSHA}, c.Stage, apiv1.WorkspaceMode(request.Attempt.Workspace))
	if err != nil {
		return request, blobs, err
	}
	request.Workspace = &childpod.WorkspaceInput{Path: workspace.Path, Fork: fork}
	return request, blobs, nil
}
