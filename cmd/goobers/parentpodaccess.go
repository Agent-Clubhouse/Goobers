package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/podauth"
)

// The pod exchanges its signed parent identity for one exact active-attempt
// grant. This bearer never enters a retained execution kit or invocation JSON.
func parentPodAccess(registry *journal.RegistryScrubber) harness.ChildWorkflowAccessProvider {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
		if env.ChildWorkflowOrigin == nil || registry == nil || !strings.HasPrefix(os.Getenv(dispatcher.EnvPodToken), podauth.WorkflowParentPodPrefix) {
			return nil, nil, errors.New("parent pod origin unavailable")
		}
		client := childworkflow.ParentAccessClient{Endpoint: os.Getenv(dispatcher.EnvDaemonAPI), Token: os.Getenv(dispatcher.EnvPodToken), ContractDigest: os.Getenv(dispatcher.EnvChildExecutionDigest)}
		access, err := client.Acquire(ctx, env.RunID)
		if err != nil {
			return nil, nil, err
		}
		registry.Register([]byte(access.BearerToken))
		closeAccess := func() error {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return client.Revoke(cleanup, env.RunID)
		}
		return access, closeAccess, nil
	}
}
