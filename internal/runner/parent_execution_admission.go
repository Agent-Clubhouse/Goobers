package runner

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// Standalone and unconfigured runners retain the refusal. Only the daemon's
// source-bound composition can admit a contained parent, and it must provide
// durable handoff, capacity, shared journal and workspace recovery owners.
func (r *Runner) admitParentExecution(ctx context.Context, machine *workflow.Machine) error {
	refusal := workflow.RefuseChildWorkflowExecution(machine.Def.Spec)
	if refusal == nil {
		return nil
	}
	if r.cfg.childExecution != nil || r.cfg.AdmitParentExecution == nil || r.cfg.ChildHandoff == nil || r.cfg.ChildParentCapacity == nil || r.cfg.BorrowParentJournal == nil || r.cfg.RestoreParentArchive == nil {
		return refusal
	}
	// Parallel parents additionally require the host-owned fork/result/join
	// lifecycle; the serial transport alone cannot provide branch custody.
	if len(machine.Def.Spec.Parallels) != 0 && (r.cfg.PrepareParentForkSource == nil || r.cfg.PrepareParentForkResult == nil || r.cfg.JoinParentFork == nil) {
		return fmt.Errorf("%w: parallel parent workspace owners unavailable", refusal)
	}
	return r.cfg.AdmitParentExecution(ctx, machine)
}

func (r *Runner) recordHostTaskPlacement(tf taskFrame, attempt int, class journal.AttemptClass) error {
	if tf.t.ChildWorkflows != nil {
		return nil
	}
	r.observeSelfExecution(false)
	if !r.recordsPlacement() {
		return nil
	}
	if err := tf.jr.Append(journal.PlacementEvent(tf.t.Name, attempt, class, selfPlacement())); err != nil {
		return fmt.Errorf("runner: journal placement for %q: %w", tf.t.Name, err)
	}
	return nil
}
