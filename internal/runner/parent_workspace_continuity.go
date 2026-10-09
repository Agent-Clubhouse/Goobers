package runner

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// inheritedParentWorkspace keeps the real checkout, including its index and
// uncommitted files, when an ordinary stage follows contained parent work.
// Source configuration, journal ownership and physical workspace identity remain
// authoritative; the invocation cannot nominate a filesystem path.
func (r *Runner) inheritedParentWorkspace(ctx context.Context, in StartInput, mode apiv1.WorkspaceMode, syncBase bool, branch string) (*stageWorkspace, error) {
	if in.Child != nil || in.Machine == nil || (mode == apiv1.WorkspaceScratch || mode == apiv1.WorkspaceRepoReadOnly) || !hasContainedParentStage(in) {
		return nil, nil
	}
	if branch == "" {
		branch = providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(r.cfg.RunsDir, in.RunID))
	if err != nil {
		return nil, err
	}
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}
	contribution, found, err := selectHeldParentContribution(events, in.RunID, branch)
	if err != nil || !found {
		return nil, err
	}
	if in.pinnedWorkspace != nil || r.cfg.Worktrees == nil || r.cfg.RepoCloneURL == nil {
		return nil, errors.New("parent contribution managed workspace unavailable")
	}
	if err := selectedWorkspaceUnsupported(in, mode); err != nil {
		return nil, err
	}
	if _, err := reader.ArtifactBytesBounded(contribution.Output, maxParentContributionBytes); err != nil {
		return nil, err
	}
	url, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return nil, err
	}
	wt, err := r.cfg.Worktrees.AdoptHeldStage(ctx, url, contribution.Custody.Workspace)
	if err != nil {
		return nil, err
	}
	base := in.RepoRef.Branch
	if base == "" {
		base = "main"
	}
	if err := wt.PrepareHeldStage(ctx, base, syncBase); err != nil {
		return nil, err
	}
	if err := verifyWorkspaceBranchSHA(ctx, in, wt, branch); err != nil {
		return nil, err
	}
	return &stageWorkspace{path: wt.Path, worktree: wt, parentContribution: true, retainedChild: func(context.Context) error { return nil }}, nil
}

func hasContainedParentStage(in StartInput) bool {
	for _, task := range in.Machine.Def.Spec.Tasks {
		if task.ChildWorkflows != nil {
			return true
		}
	}
	return false
}

// Select by the physical branch, never the latest sibling's journal event. A
// newer hold without matching imported output fences reuse: a finished stage
// alone does not establish that a remote writer returned its workspace.
func selectHeldParentContribution(events []journal.Event, runID, branch string) (parentContribution, bool, error) {
	var result parentContribution
	var hold ContainedParentWorkspaceCustody
	var heldAt, returnedAt uint64
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		switch event.Runner["kind"] {
		case ContainedParentWorkspaceKind:
			var value ContainedParentWorkspaceCustody
			data, err := json.Marshal(event.Runner["custody"])
			if err != nil || len(data) > 8192 || json.Unmarshal(data, &value) != nil || value.Version != 1 || value.Origin == nil || value.Workspace.OwnerRunID != runID {
				return result, false, errors.New("invalid retained parent workspace")
			}
			if value.Workspace.Branch == branch {
				hold, heldAt = value, event.Seq
			}
		case ParentContributionKind:
			value, err := decodeParentContribution(event)
			if err != nil {
				return result, false, err
			}
			if value.Custody.Workspace.OwnerRunID != runID {
				return result, false, errors.New("parent contribution belongs to another run")
			}
			if value.Custody.Workspace.Branch == branch {
				if heldAt == 0 || event.Seq <= heldAt || value.Custody.Workspace != hold.Workspace || *value.Custody.Origin != *hold.Origin {
					return result, false, errors.New("parent contribution differs from retained workspace")
				}
				result, returnedAt = value, event.Seq
			}
		case ParentContributionRetiredKind:
			value, err := decodeParentContribution(event)
			if err != nil {
				return result, false, err
			}
			if value.Custody.Workspace.Branch == branch {
				return result, false, errors.New("parent contribution has been retired")
			}
		}
	}
	if heldAt == 0 {
		return result, false, nil
	}
	if returnedAt <= heldAt {
		return result, false, errors.New("parent workspace has no acknowledged return for its latest owner")
	}
	return result, true, nil
}
