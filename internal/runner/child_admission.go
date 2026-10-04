package runner

import (
	"fmt"

	"github.com/goobers/goobers/internal/journal"

	"github.com/goobers/goobers/internal/workflow"
)

// admitChildWorkflows keeps the default refusal and delegates only to a
// trusted host backend that can prove the complete parent execution topology.
// A configured task policy alone never enables execution.
func (r *Runner) admitChildWorkflows(machine *workflow.Machine) error {
	if machine == nil {
		return nil
	}
	refused := workflow.RefuseChildWorkflowExecution(machine.Def.Spec)
	if refused == nil {
		return nil
	}
	if r.cfg.ChildWorkflowAdmission == nil {
		return refused
	}
	if r.cfg.ChildHandoff == nil || r.cfg.ChildParentCapacity == nil || r.cfg.ChildWorkflowRecoveryAdmission == nil {
		return fmt.Errorf("%w: parent custody, recovery or capacity service unavailable", refused)
	}
	return r.cfg.ChildWorkflowAdmission(machine)
}

func (r *Runner) containedParentTask(tf taskFrame) bool {
	return tf.t.ChildWorkflows != nil && r.cfg.ChildWorkflowAdmission != nil && r.admitChildWorkflows(tf.in.Machine) == nil
}

func (r *Runner) verifyChildWorkflowCustody(dir string) error {
	if r.cfg.ChildWorkflowRecoveryAdmission == nil {
		return nil
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return err
	}
	return r.cfg.ChildWorkflowRecoveryAdmission(reader)
}

func (r *Runner) localTaskDenied(tf taskFrame) bool {
	return r.cfg.SelfExecutionDenied && !r.containedParentTask(tf)
}
func (r *Runner) taskCustodyReady(tf taskFrame) error {
	if tf.t.ChildWorkflows == nil {
		return nil
	}
	return r.verifyChildWorkflowCustody(tf.jr.Dir())
}

// The contained worker reports its actual placement after transport. Ordinary
// self execution retains the existing zero-declaration journal behavior.
func (r *Runner) recordTaskPlacement(tf taskFrame, attempt int, class journal.AttemptClass) error {
	if r.containedParentTask(tf) {
		return nil
	}
	r.observeSelfExecution(false)
	if !r.recordsPlacement() {
		return nil
	}
	return tf.jr.Append(journal.PlacementEvent(tf.t.Name, attempt, class, selfPlacement()))
}
