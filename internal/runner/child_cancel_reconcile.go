package runner

import (
	"errors"
	"reflect"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// FinalizeCancelledChild records cancellation of an ownerless child whose
// physical workers have already stopped and returned their data. The caller
// must hold exclusive execution custody and the journal writer lock, and must
// independently verify the durable family cancellation and physical-worker
// joins. Repository writer proof is checked again here. No workflow is resumed,
// no credentials are acquired, and no provider finalizers are invoked: generated
// children retain their work for the queue's result/disposition lifecycle, just
// as ForChildExecution excludes ordinary backlog and provider finalizers.
func FinalizeCancelledChild(reader *journal.Reader, writer *journal.Run, id journal.RunIdentity, now time.Time) (Result, error) {
	if reader == nil || writer == nil || reader.Dir() != writer.Dir() || id.Child == nil || id.ValidateChildLineage() != nil || now.IsZero() {
		return Result{}, errors.New("runner: cancelled child custody is incomplete")
	}
	actual, err := reader.Identity()
	if err != nil || !reflect.DeepEqual(actual, id) {
		return Result{}, errors.Join(errors.New("runner: cancelled child identity differs from journal"), err)
	}
	events, err := reader.Events()
	if err != nil {
		return Result{}, err
	}
	if phase := journal.PhaseFromEvents(events); phase != journal.PhaseRunning {
		return Result{Phase: phase}, nil
	}
	admission, err := PinnedChildWorkspaceAdmission(reader, id)
	if err != nil {
		return Result{}, err
	}
	if admission != nil {
		if err := VerifyChildWorkspaceQuiescence(reader, id, events); err != nil {
			return Result{}, err
		}
	}
	// This terminal-only runner has no stage factories, worktree manager or
	// provider hooks. Reuse the ordinary cancellation diagnostic/terminal cause
	// construction without creating any execution path or deleting child work.
	terminalizer := &Runner{}
	return terminalizer.finishStalledTakeover(id.RunID, writer, "", 0, stalledRequest{kind: interruptCancel, now: now, phase: journal.PhaseAborted, cause: errCanceledRun})
}
