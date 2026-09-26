package runner

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func (in StartInput) configuredRepository() apiv1.RepoRef {
	if in.configuredRepoRef != nil {
		return *in.configuredRepoRef
	}
	return in.RepoRef
}

func (in *StartInput) pinConfiguredRepository() {
	base := in.configuredRepository()
	in.configuredRepoRef = base.DeepCopy()
}

func (r *Runner) resolveWorkspaceRevision(ctx context.Context, revision apiv1.WorkspaceRevision, base apiv1.RepoRef) (apiv1.RepoRef, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return workspacerevision.Resolve(lookupCtx, revision, base, r.cfg.AdditionalRepos, r.cfg.ResolveRepositoryIdentity)
}

func recordWorkspaceRevisionRejection(jr executionJournal, stage string, attempt int, class journal.AttemptClass, rejection *workspacerevision.Error) error {
	if err := jr.Append(journal.Event{
		Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: class,
		Error: &journal.ErrorDetail{Code: rejection.Code, Message: rejection.Error()},
	}); err != nil {
		return fmt.Errorf("runner: journal workspace revision rejection: %w", err)
	}
	return rejection
}

func (r *Runner) restoreWorkspaceRevision(ctx context.Context, in StartInput, events []journal.Event) (StartInput, error) {
	in.pinConfiguredRepository()
	revision, err := reconstructWorkspaceRevision(events, in.Machine)
	if err != nil {
		return in, err
	}
	revision, err = workspacerevision.Accept(in.workspaceRevision, revision)
	if err != nil {
		return in, err
	}
	if revision != nil {
		in.RepoRef, err = r.resolveWorkspaceRevision(ctx, *revision, in.configuredRepository())
		if err != nil {
			return in, fmt.Errorf("runner: resolve persisted workspace revision repository: %w", err)
		}
	}
	in.workspaceRevision = revision.DeepCopy()
	return in, nil
}

func (r *Runner) restoreResumeWorkspaceRevision(ctx context.Context, in StartInput, events []journal.Event, parallel *parallelExec, start, branch int) (StartInput, error) {
	if err := r.validateWorkspaceRevisionHistory(ctx, in, events); err != nil {
		return in, err
	}
	rootEvents := events
	if parallel != nil {
		rootEvents = events[:start]
	}
	in, err := r.restoreWorkspaceRevision(ctx, in, rootEvents)
	if err != nil || parallel == nil || branch == 0 {
		return in, err
	}
	return r.restoreWorkspaceRevision(ctx, in, newParallelBranchEventIndex(events, parallel.spec.Name).events(branch))
}

func (r *Runner) validateWorkspaceRevisionHistory(ctx context.Context, in StartInput, events []journal.Event) error {
	for _, event := range events {
		if event.WorkspaceRevision == nil {
			continue
		}
		revision, err := reconstructWorkspaceRevision([]journal.Event{event}, in.Machine)
		if err != nil {
			return err
		}
		if _, err := r.resolveWorkspaceRevision(ctx, *revision, in.configuredRepository()); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) restoreSerialParallelRevision(ctx context.Context, ws *walkState, more bool) error {
	reader, err := journal.OpenRead(ws.jr.Dir())
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	in := ws.in
	in.RepoRef, in.workspaceRevision = in.configuredRepository(), nil
	if more {
		rootEvents, ok := parallelRootEvents(events, ws.parallel.spec.Name)
		if !ok {
			return fmt.Errorf("runner: missing parallel %q history", ws.parallel.spec.Name)
		}
		in, err = r.restoreWorkspaceRevision(ctx, in, rootEvents)
	} else {
		in, err = r.restoreWorkspaceRevision(ctx, in, events)
	}
	if err == nil {
		ws.in = in
	}
	return err
}

func selectedWorkspaceUnsupported(in StartInput, mode apiv1.WorkspaceMode) error {
	if in.workspaceRevision == nil || (mode == apiv1.WorkspaceScratch && in.pinnedWorkspace == nil) {
		return nil
	}
	return &workspacerevision.Error{
		Code:    workspacerevision.CodeInvalid,
		Message: "selected-revision repository workspace provisioning is not supported yet",
	}
}
