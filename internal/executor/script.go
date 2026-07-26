package executor

import (
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func deterministicCommand(run apiv1.DeterministicRun) ([]string, []string, func(), error) {
	if run.Command != nil && run.Script != "" {
		return nil, nil, nil, errors.New("executor: DeterministicRun declares both command and script")
	}
	if run.Script != "" {
		return scriptCommand(run.Script)
	}
	if len(run.Command) == 0 {
		return nil, nil, nil, errors.New("executor: DeterministicRun declares no command or script")
	}
	return run.Command, nil, func() {}, nil
}
