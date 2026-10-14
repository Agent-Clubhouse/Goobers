package runner

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func (r *Runner) borrowParentJournal(run *journal.Run) (func(), error) {
	noop := func() {}
	if r.cfg.BorrowParentJournal == nil {
		return noop, nil
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		return nil, err
	}
	id, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	if id.Child != nil {
		return nil, errors.New("runner: child cannot borrow parent journal ownership")
	}
	machine, err := PinnedWorkflowMachine(reader, id)
	if err != nil {
		// Resume owns diagnostics/terminalization for missing or malformed
		// historical definitions. Unknown provenance grants no remote writer.
		return noop, nil
	}
	if workflow.RefuseChildWorkflowExecution(machine.Def.Spec) == nil {
		return noop, nil
	}
	release, err := r.cfg.BorrowParentJournal(id.RunID, id.Gaggle, run)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, errors.New("runner: parent journal owner omitted release")
	}
	return release, nil
}
