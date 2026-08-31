package executor

import (
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// DeterministicCommand builds the argv, extra command-scoped env, and
// cleanup for one DeterministicRun's Command or Script. Exported so the
// mode-3 in-pod executor (cmd/goobers's dispatch-exec entrypoint) builds an
// identical argv to the local executor for the same declaration, rather than
// a second implementation that could drift.
func DeterministicCommand(run apiv1.DeterministicRun) ([]string, []string, func(), error) {
	if run.Command != nil && run.Script != "" {
		return nil, nil, nil, errors.New("executor: DeterministicRun declares both command and script")
	}
	if run.Script != "" {
		return scriptCommand(run.Script)
	}
	if len(run.Command) == 0 {
		return nil, nil, nil, errors.New("executor: DeterministicRun declares no command or script")
	}
	if strings.TrimSpace(run.Command[0]) == "" {
		return nil, nil, nil, fmt.Errorf(
			"executor: DeterministicRun command[0] must name a non-whitespace executable, got %q",
			run.Command[0],
		)
	}
	return run.Command, nil, func() {}, nil
}
