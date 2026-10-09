package runner

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// ParentArchiveRestorer runs under the owning runner's journal/execution lease.
// The host resolves the verified archive from bounded inventory, persists apply
// intent, restores and holds the exact managed checkout, then acknowledges with
// RecordParentArchiveRestoration. Returning nil without that receipt is failure.
type ParentArchiveRestorer func(context.Context, OwnedJournalRecorder, ParentWorkspaceArchive, uint64) error

// Complete admission and recover physical custody before constructing any
// executor. Both new walks and resumed walks use this same boundary.
func (r *Runner) prepareWalkExecution(ctx context.Context, ws *walkState) error {
	if err := workflow.RefuseChildWorkflowExecution(ws.in.Machine.Def.Spec); err != nil {
		return err
	}
	return r.restoreParentWorkspaceArchives(ctx, ws.jr)
}

func (r *Runner) restoreParentWorkspaceArchives(ctx context.Context, run *journal.Run) error {
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	branches, err := parentArchiveBranches(events)
	if err != nil {
		return err
	}
	for _, branch := range branches {
		recorder, err := OwnedBranchRecorder(run, branch.index)
		if err != nil {
			return err
		}
		state, err := ownedParentArchiveState(recorder, branch.name)
		if err != nil {
			return err
		}
		if state.retiredAt == 0 {
			continue
		}
		if r.cfg.RestoreParentArchive == nil {
			return errors.New("parent archive restoration service unavailable")
		}
		if err := r.cfg.RestoreParentArchive(ctx, recorder, state.archive, state.retiredAt); err != nil {
			return err
		}
		verified, err := ownedParentArchiveState(recorder, branch.name)
		if err != nil {
			return err
		}
		if verified.retiredAt != 0 || verified.restoredRetirement != state.retiredAt {
			return errors.New("parent archive restoration was not durably acknowledged")
		}
	}
	return nil
}

type parentArchiveBranch struct {
	name  string
	index int
}

func parentArchiveBranches(events []journal.Event) ([]parentArchiveBranch, error) {
	var branches []parentArchiveBranch
	seen := map[string]int{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentContributionRetiredKind {
			continue
		}
		value, err := decodeParentWorkspaceArchive(event)
		if err != nil {
			return nil, err
		}
		name := value.Custody.Workspace.Branch
		if prior, exists := seen[name]; exists {
			if prior != event.Branch {
				return nil, errors.New("parent archive branch attribution changed")
			}
			continue
		}
		if len(branches) >= 128 || event.Branch < 0 || event.Branch > 128 {
			return nil, errors.New("parent archive branch limit exceeded")
		}
		seen[name] = event.Branch
		branches = append(branches, parentArchiveBranch{name: name, index: event.Branch})
	}
	return branches, nil
}
