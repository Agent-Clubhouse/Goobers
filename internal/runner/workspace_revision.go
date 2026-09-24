package runner

import (
	"fmt"

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

func (r *Runner) restoreWorkspaceRevision(in StartInput, events []journal.Event) (StartInput, error) {
	in.pinConfiguredRepository()
	revision, err := reconstructWorkspaceRevision(events, in.Machine)
	if err != nil {
		return in, err
	}
	revision, err = workspacerevision.Accept(in.workspaceRevision, revision, true, true)
	if err != nil {
		return in, err
	}
	if revision != nil {
		in.RepoRef, err = workspacerevision.Resolve(*revision, in.configuredRepository(), r.cfg.AdditionalRepos)
		if err != nil {
			return in, fmt.Errorf("runner: resolve persisted workspace revision repository: %w", err)
		}
	}
	in.workspaceRevision = revision.DeepCopy()
	return in, nil
}

func (r *Runner) restoreResumeWorkspaceRevision(in StartInput, events []journal.Event, parallel *parallelExec, start, branch int) (StartInput, error) {
	if err := r.validateWorkspaceRevisionHistory(in, events); err != nil {
		return in, err
	}
	rootEvents := events
	if parallel != nil {
		rootEvents = events[:start]
	}
	in, err := r.restoreWorkspaceRevision(in, rootEvents)
	if err != nil || parallel == nil || branch == 0 {
		return in, err
	}
	return r.restoreWorkspaceRevision(in, newParallelBranchEventIndex(events, parallel.spec.Name).events(branch))
}

func (r *Runner) validateWorkspaceRevisionHistory(in StartInput, events []journal.Event) error {
	for _, event := range events {
		if event.WorkspaceRevision == nil {
			continue
		}
		revision, err := reconstructWorkspaceRevision([]journal.Event{event}, in.Machine)
		if err != nil {
			return err
		}
		if _, err := workspacerevision.Resolve(*revision, in.configuredRepository(), r.cfg.AdditionalRepos); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) restoreSerialParallelRevision(ws *walkState, more bool) error {
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
		in, err = r.restoreWorkspaceRevision(in, rootEvents)
	} else {
		in, err = r.restoreWorkspaceRevision(in, events)
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
