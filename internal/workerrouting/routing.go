// Package workerrouting checks whether the "goobers" Temporal worker
// deployment routes workflow tasks to the workers actually polling for them
// (#5950, #6020). A versioned worker whose build is not the deployment's
// current (or ramping) version polls successfully and receives nothing, so a
// stalled engine shows a Ready worker, zero poll errors and zero dispatch.
//
// The running worker reports its own routing in its log (internal/workerhost);
// `goobers status` and `goobers doctor --k8s` assess the live fleet with the
// same rule. Nothing here changes the routing: setting the current version is
// an operator action (#4084).
package workerrouting

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/client"
)

// DeploymentName is the Temporal worker deployment a versioned worker
// registers under.
const DeploymentName = "goobers"

// UnversionedLabel names the deployment's routing target when its current
// version is unversioned workers, in the Temporal CLI's own spelling.
const UnversionedLabel = "__unversioned__"

// Routing is the deployment's routing config plus which versions it reports
// as draining.
type Routing struct {
	client.WorkerDeploymentRoutingConfig
	// Draining holds the build IDs whose versions are draining or drained:
	// they were current or ramping and no longer are, so they receive no new
	// workflows by design while their pinned workflows finish.
	Draining map[string]bool
}

// Finding is one worker's routing verdict. The zero value is routed.
type Finding struct {
	// Problem is the operator-facing sentence the worker logs; "" when the
	// deployment routes new tasks to the worker or the worker is draining.
	Problem string
	// Command is the Temporal CLI invocation that routes new tasks to live
	// workers; "" when there is no problem or routing could not be read.
	Command string
	// Unverified marks a versioned worker whose deployment could not be
	// described: the routing is unknown, not known to be wrong.
	Unverified bool
	// Draining marks a worker whose version was replaced: it receives no new
	// workflows, which is expected, not a problem.
	Draining bool
}

// Routed reports whether the routing is no problem for the worker: it
// receives a share of new workflow tasks, or it is draining.
func (f Finding) Routed() bool { return f.Problem == "" }

// DescribeRouting reads the "goobers" worker deployment: which versions
// receive new workflows (and the tasks of unpinned ones), in what shares, and
// which versions are draining.
func DescribeRouting(ctx context.Context, c client.Client) (Routing, error) {
	if c == nil {
		return Routing{}, errors.New("no temporal client")
	}
	resp, err := c.WorkerDeploymentClient().GetHandle(DeploymentName).Describe(ctx, client.WorkerDeploymentDescribeOptions{})
	if err != nil {
		return Routing{}, err
	}
	routing := Routing{WorkerDeploymentRoutingConfig: resp.Info.RoutingConfig}
	for _, summary := range resp.Info.VersionSummaries {
		if summary.Version.DeploymentName != DeploymentName {
			continue
		}
		switch summary.DrainageStatus {
		case client.WorkerDeploymentVersionDrainageStatusDraining, client.WorkerDeploymentVersionDrainageStatusDrained:
			if routing.Draining == nil {
				routing.Draining = map[string]bool{}
			}
			routing.Draining[summary.Version.BuildID] = true
		}
	}
	return routing, nil
}

// Check explains why the deployment's routing sends a worker no new tasks
// (#5950). buildID is the worker's build; versioned reports whether it polls
// as version goobers.<buildID> rather than unversioned. It is the rule the
// fleet assessment applies to every live build: a worker is routed when its
// version receives a share of new workflow tasks (the current version gets
// 100% minus the ramp, the ramping version the ramp), and a draining version
// receiving none is expected.
//
// A describe error is a problem only for a versioned worker. An unversioned
// worker on a namespace with versioning disabled, or where no versioned worker
// ever registered, cannot describe the deployment and needs nothing from it.
func Check(versioned bool, buildID string, routing Routing, describeErr error) Finding {
	if describeErr != nil {
		if !versioned {
			return Finding{}
		}
		return Finding{
			Problem: fmt.Sprintf("cannot verify that worker deployment %q routes tasks to this worker (build %s): %v",
				DeploymentName, buildID, describeErr),
			Unverified: true,
		}
	}
	served := version{}
	if versioned {
		served = version{versioned: true, build: buildID}
	}
	plan := planTraffic(routing.WorkerDeploymentRoutingConfig, nil)
	if plan.shareOf(served) > 0 {
		return Finding{}
	}
	if versioned && routing.Draining[buildID] {
		return Finding{Draining: true}
	}
	if served == plan.current.version && plan.ramp != nil {
		command := plan.ramp.promoteCommand()
		return Finding{
			Problem: fmt.Sprintf("worker deployment %q ramps %s of new workflow tasks to %s, so this worker (%s, the current version) receives none. "+
				"Finish the rollout: %s", DeploymentName, percent(plan.ramp.share), plan.ramp.label(), served.label(), command),
			Command: command,
		}
	}
	command := served.promoteCommand()
	if !versioned {
		return Finding{
			Problem: fmt.Sprintf("worker deployment %q current version is %s, so this UNVERSIONED worker receives no new workflow tasks "+
				"(engine.workerVersioning is off). Either set engine.workerVersioning: true, or point the deployment at unversioned workers: %s",
				DeploymentName, plan.currentDescription(), command),
			Command: command,
		}
	}
	return Finding{
		Problem: fmt.Sprintf("worker deployment %q current version is %s but this worker serves %s, so it receives no new workflow tasks "+
			"and the engine stalls. Make this build current: %s",
			DeploymentName, plan.currentDescription(), served.label(), command),
		Command: command,
	}
}

// BuildCommand is the Temporal CLI invocation that makes build buildID the
// deployment's current version.
func BuildCommand(buildID string) string {
	return fmt.Sprintf("temporal worker deployment set-current-version --deployment-name %s --build-id %s", DeploymentName, buildID)
}

// UnversionedCommand is the Temporal CLI invocation that routes the
// deployment's new workflow tasks to unversioned workers.
func UnversionedCommand() string {
	return fmt.Sprintf("temporal worker deployment set-current-version --deployment-name %s --unversioned", DeploymentName)
}
