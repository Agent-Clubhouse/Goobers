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
	"go.temporal.io/sdk/worker"
)

// DeploymentName is the Temporal worker deployment a versioned worker
// registers under.
const DeploymentName = "goobers"

// UnversionedLabel names the deployment's routing target when its current
// version is unversioned workers, in the Temporal CLI's own spelling.
const UnversionedLabel = "__unversioned__"

// Finding is one worker's routing verdict. The zero value is routed.
type Finding struct {
	// Problem is the operator-facing sentence the worker logs; "" when the
	// deployment routes tasks to the worker.
	Problem string
	// Command is the set-current-version invocation that routes tasks to the
	// worker; "" when it is routed or when routing could not be read.
	Command string
	// Unverified marks a versioned worker whose deployment could not be
	// described: the routing is unknown, not known to be wrong.
	Unverified bool
}

// Routed reports whether the deployment sends the worker tasks.
func (f Finding) Routed() bool { return f.Problem == "" }

// DescribeRouting reads the "goobers" worker deployment's routing config:
// which version receives new workflows and the tasks of unpinned ones.
func DescribeRouting(ctx context.Context, c client.Client) (client.WorkerDeploymentRoutingConfig, error) {
	if c == nil {
		return client.WorkerDeploymentRoutingConfig{}, errors.New("no temporal client")
	}
	resp, err := c.WorkerDeploymentClient().GetHandle(DeploymentName).Describe(ctx, client.WorkerDeploymentDescribeOptions{})
	if err != nil {
		return client.WorkerDeploymentRoutingConfig{}, err
	}
	return resp.Info.RoutingConfig, nil
}

// Check explains why the deployment's routing sends a worker no tasks (#5950).
// buildID is the worker's build; versioned reports whether it polls as
// version goobers.<buildID> rather than unversioned.
//
// A describe error is a problem only for a versioned worker. An unversioned
// worker on a namespace with versioning disabled, or where no versioned worker
// ever registered, cannot describe the deployment and needs nothing from it.
func Check(versioned bool, buildID string, routing client.WorkerDeploymentRoutingConfig, describeErr error) Finding {
	if !versioned {
		if describeErr != nil || routing.CurrentVersion == nil {
			return Finding{}
		}
		command := UnversionedCommand()
		return Finding{
			Problem: fmt.Sprintf("worker deployment %q current version is %s, so this UNVERSIONED worker receives no new workflow tasks "+
				"(engine.workerVersioning is off). Either set engine.workerVersioning: true, or point the deployment at unversioned workers: %s",
				DeploymentName, VersionLabel(routing.CurrentVersion), command),
			Command: command,
		}
	}
	if describeErr != nil {
		return Finding{
			Problem: fmt.Sprintf("cannot verify that worker deployment %q routes tasks to this worker (build %s): %v",
				DeploymentName, buildID, describeErr),
			Unverified: true,
		}
	}
	if servesVersion(routing.CurrentVersion, buildID) {
		return Finding{}
	}
	if servesVersion(routing.RampingVersion, buildID) && routing.RampingVersionPercentage > 0 {
		return Finding{}
	}
	command := BuildCommand(buildID)
	return Finding{
		Problem: fmt.Sprintf("worker deployment %q current version is %s but this worker serves %s.%s, so it receives no new workflow tasks "+
			"and the engine stalls. Make this build current: %s",
			DeploymentName, VersionLabel(routing.CurrentVersion), DeploymentName, buildID, command),
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

func servesVersion(v *worker.WorkerDeploymentVersion, buildID string) bool {
	return v != nil && v.DeploymentName == DeploymentName && v.BuildID == buildID
}

// VersionLabel renders a deployment version as <deployment>.<build>, or
// UnversionedLabel for nil.
func VersionLabel(v *worker.WorkerDeploymentVersion) string {
	if v == nil {
		return UnversionedLabel
	}
	return v.DeploymentName + "." + v.BuildID
}
