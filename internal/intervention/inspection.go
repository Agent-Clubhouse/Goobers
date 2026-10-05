package intervention

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// Inspection can recover immutable source metadata after child execution
// authority has ended. It intentionally carries no executable Runner. Legacy
// actions keep resolve(), so they cannot use this path to mutate old execution.
func (s *Service) resolveRunDefinition(id journal.RunIdentity, definitions Definitions, fallback *runner.Runner, inspection bool) (Execution, error) {
	if !inspection || id.Child == nil {
		return s.interventionExecution(id, definitions, fallback)
	}
	if s.pinnedInspection == nil {
		return Execution{}, errors.New("retained child inspection unavailable")
	}
	inspected, err := s.pinnedInspection(context.Background(), id)
	if err != nil {
		return Execution{}, err
	}
	if inspected.Runner != nil || inspected.ChildRestart != nil || inspected.Machine == nil || inspected.Machine.Digest() != id.WorkflowDigest || inspected.GooberDigest != id.GooberDigest {
		return Execution{}, errors.New("retained child inspection has invalid source pins or executable authority")
	}
	return inspected, nil
}
