package runner

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
)

func (r *Runner) retireParentWorkspaces(run *journal.Run) error {
	if r.cfg.RetireParentWorkspaces == nil {
		return nil
	}
	err := r.cfg.RetireParentWorkspaces(run)
	if err == nil {
		return nil
	}
	return errors.Join(err, run.Append(journal.Event{Type: journal.EventError, Error: journal.ErrorDetailFor("parent_workspace_retirement_failed", err)}))
}
