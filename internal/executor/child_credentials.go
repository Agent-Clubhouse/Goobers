package executor

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

func validateShellChildWorkspace(ctx context.Context, env apiv1.InvocationEnvelope) error {
	if err := credentials.RefuseUnisolatedChildProcess(ctx); err != nil {
		return err
	}
	// An empty exec.Cmd.Dir silently chooses the daemon directory.
	if env.Workspace == "" {
		return errors.New("executor: InvocationEnvelope.Workspace is empty")
	}
	return nil
}
