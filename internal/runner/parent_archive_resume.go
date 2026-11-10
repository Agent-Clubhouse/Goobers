package runner

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspaceauthority"
)

// ParentArchiveRestorer runs under the owning runner's journal/execution lease.
// The host resolves the verified archive from bounded inventory, persists apply
// intent, restores and holds the exact managed checkout, then acknowledges with
// RecordParentArchiveRestoration. Returning nil without that receipt is failure.
type ParentArchiveRestorer func(context.Context, OwnedJournalRecorder, ParentWorkspaceArchive, uint64) error

// Complete admission and recover physical custody before constructing any
// executor. Both new walks and resumed walks use this same boundary.
func (r *Runner) prepareWalkExecution(ctx context.Context, ws *walkState) error {
	if err := r.admitExecution(ctx, ws.in.Machine); err != nil {
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
		recorder, err := OwnedBranchRecorder(run, branch.Index)
		if err != nil {
			return err
		}
		state, err := ownedParentArchiveState(recorder, branch.Name)
		if err != nil {
			return err
		}
		if state.RetiredAt == 0 {
			continue
		}
		if r.cfg.RestoreParentArchive == nil {
			return errors.New("parent archive restoration service unavailable")
		}
		if err := r.cfg.RestoreParentArchive(ctx, recorder, state.Archive, state.RetiredAt); err != nil {
			return err
		}
		verified, err := ownedParentArchiveState(recorder, branch.Name)
		if err != nil {
			return err
		}
		if verified.RetiredAt != 0 || verified.RestoredRetirement != state.RetiredAt {
			return errors.New("parent archive restoration was not durably acknowledged")
		}
	}
	return nil
}

type parentArchiveBranch = workspaceauthority.ParentArchiveBranch

func parentArchiveBranches(events []journal.Event) ([]parentArchiveBranch, error) {
	return workspaceauthority.ParentArchiveBranches(events)
}
