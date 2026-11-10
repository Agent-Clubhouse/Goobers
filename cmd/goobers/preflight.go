package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
)

// preflightDaemonHarnesses converts the shared preflight's structured error
// into daemon admission state. Keeping that policy at this boundary lets the
// worker continue treating the same error as fatal at its own narrower scope.
// The returned failures are nil when every harness is usable; a refusal whose
// every cause was transient carries a recheck that re-admits the workflow once
// the provider recovers (#6017).
func preflightDaemonHarnesses(
	goobers map[string]apiv1.GooberSpec,
	workflows []apiv1.Workflow,
	environment harness.EnvironmentConfig,
	harnessCommand map[string][]string,
	credentialFor runtimewiring.ModelCredentialFor,
	warnings io.Writer,
) (runtimewiring.HarnessPreflightInfo, *runtimewiring.HarnessPreflightFailures, error) {
	info, err := runtimewiring.PreflightHarnesses(goobers, workflows, environment, harnessCommand, credentialFor)
	if err == nil {
		return info, nil, nil
	}
	var failures *runtimewiring.HarnessPreflightFailures
	if !errors.As(err, &failures) {
		return nil, nil, err
	}
	for _, identity := range sortedWorkflowIdentities(failures.Refusals) {
		_, _ = fmt.Fprintf(warnings, "warning: workflow %q (gaggle %q) is refused because a required harness is unavailable: %s\n",
			identity.Workflow, identity.Gaggle, failures.Refusals[identity])
	}
	return info, failures, nil
}

func preflightSchedulerHarnesses(cfg *instance.Config, set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec, stores credentials.StoreResolver) (runtimewiring.HarnessPreflightInfo, *runtimewiring.HarnessPreflightFailures, error) {
	return preflightDaemonHarnesses(
		goobers, set.Workflows, runtimewiring.HarnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand,
		runtimewiring.HarnessModelCredentialResolver(cfg, stores), os.Stderr,
	)
}
