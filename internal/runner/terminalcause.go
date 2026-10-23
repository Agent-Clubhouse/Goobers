package runner

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workflow"
)

// dispatchTerminalError retains the actual exhausted dispatch budget without
// inferring exhaustion from an ordinary ResultFailure or an attempt number.
type dispatchTerminalError struct {
	err             error
	stage           string
	class           journal.AttemptClass
	attempts, limit int
}

func (e *dispatchTerminalError) Error() string { return e.err.Error() }
func (e *dispatchTerminalError) Unwrap() error { return e.err }

// captureTerminalCause runs before best-effort cleanup can append unrelated
// errors. Explicit failure paths supply their causal event; gate/task terminal
// paths use the last decision for finalState in the current lifecycle.
func captureTerminalCause(jr *journal.Run, phase journal.RunPhase, finalState string, supplied *journal.TerminalCause) (*journal.TerminalCause, error) {
	if phase == journal.PhaseCompleted {
		return nil, nil
	}
	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return nil, err
	}
	events, err := rd.Events()
	if err != nil {
		return nil, err
	}
	id, err := rd.Identity()
	if err != nil {
		return nil, err
	}
	var machine *workflow.Machine
	for _, input := range id.Inputs {
		if input.Name != journal.PinnedWorkflowDefinitionInputName {
			continue
		}
		machine, err = runsemantics.PinnedWorkflowMachine(rd, id)
		if err != nil {
			return nil, err
		}
		break
	}
	return runsemantics.BuildTerminalCause(events, phase, finalState, supplied, machine, id.RunControls, rd.ArtifactBytes)
}
