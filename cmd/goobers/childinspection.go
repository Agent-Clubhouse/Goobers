package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func (s *daemonCredentialService) inspectChildExecution(ctx context.Context, id journal.RunIdentity) (intervention.Execution, error) {
	if s == nil {
		return intervention.Execution{}, errors.New("child inspection unavailable")
	}
	if _, err := retainedChildExecutionRef(ctx, s.childQueue, id, false); err != nil {
		return intervention.Execution{}, err
	}
	dir, err := s.layout.FindRunDir(id.RunID)
	if err != nil {
		return intervention.Execution{}, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return intervention.Execution{}, err
	}
	machine, err := runner.PinnedWorkflowMachine(reader, id)
	if err != nil {
		return intervention.Execution{}, err
	}
	inspected := intervention.Execution{Machine: machine, GooberDigest: id.GooberDigest}
	if id.WorkspaceRepository != nil {
		inspected.RepoRef = *id.WorkspaceRepository.DeepCopy()
	}
	return inspected, nil
}
