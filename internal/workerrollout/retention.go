// Package workerrollout coordinates Temporal worker-version retention without
// taking ownership of current or ramping routing from Worker Controller.
package workerrollout

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/workerrouting"
)

const (
	// DefaultVersionLimit keeps the current version and two predecessors.
	DefaultVersionLimit   = 3
	versionStatusDraining = "Draining"
)

// DeploymentStatus is the Worker Controller state needed to make room for a
// target version that its hard version cap has blocked.
type DeploymentStatus struct {
	TargetBuildID       string
	TargetHasDeployment bool
	CurrentBuildID      string
	VersionCount        int
	Generation          int64
	ObservedGeneration  int64
	Deprecated          []DeprecatedVersion
}

// DeprecatedVersion is one version the Worker Controller is phasing out.
type DeprecatedVersion struct {
	BuildID             string
	Status              string
	EligibleForDeletion bool
}

// StatusSource reads one WorkerDeployment custom resource.
type StatusSource interface {
	Status(context.Context) (DeploymentStatus, error)
}

// Execution identifies one open pinned workflow.
type Execution struct {
	WorkflowID string
	RunID      string
}

// TemporalClient is the Temporal surface needed by retention reconciliation.
type TemporalClient interface {
	DescribeRouting(context.Context) (workerrouting.Routing, error)
	ListPinned(context.Context, string, []byte) ([]Execution, []byte, error)
	SetAutoUpgrade(context.Context, Execution) error
}

// Result describes one retention reconciliation.
type Result struct {
	RetiredBuild string
	Updated      int
}

// Reconcile makes room for a blocked target by changing workflows pinned to
// the oldest draining version to AUTO_UPGRADE. Their next workflow task runs
// on the current version, allowing the old version to become drained.
func Reconcile(ctx context.Context, source StatusSource, temporal TemporalClient, limit int) (Result, error) {
	if limit < 2 {
		return Result{}, fmt.Errorf("worker rollout: version limit %d must be at least 2", limit)
	}
	status, err := source.Status(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("worker rollout: read WorkerDeployment status: %w", err)
	}
	if !targetBlocked(status, limit) {
		return Result{}, nil
	}
	routing, err := temporal.DescribeRouting(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("worker rollout: describe Temporal routing: %w", err)
	}
	buildID, ok := oldestDraining(status, routing)
	if !ok {
		return Result{}, errors.New("worker rollout: target is blocked at the version cap but no draining predecessor is safe to retire")
	}
	updated, err := setPinnedAutoUpgrade(ctx, temporal, buildID)
	if err != nil {
		return Result{}, err
	}
	return Result{RetiredBuild: buildID, Updated: updated}, nil
}

func targetBlocked(status DeploymentStatus, limit int) bool {
	return status.Generation > 0 &&
		status.Generation == status.ObservedGeneration &&
		status.VersionCount >= limit &&
		status.TargetBuildID != "" &&
		status.TargetBuildID != status.CurrentBuildID &&
		!status.TargetHasDeployment
}

func oldestDraining(status DeploymentStatus, routing workerrouting.Routing) (string, bool) {
	candidates := make([]string, 0, len(status.Deprecated))
	for _, version := range status.Deprecated {
		if version.BuildID == "" || version.EligibleForDeletion || version.Status != versionStatusDraining {
			continue
		}
		if routing.Created[version.BuildID].IsZero() {
			continue
		}
		candidates = append(candidates, version.BuildID)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return routing.Created[candidates[i]].Before(routing.Created[candidates[j]])
	})
	if len(candidates) == 0 {
		return "", false
	}
	return candidates[0], true
}

func setPinnedAutoUpgrade(ctx context.Context, temporal TemporalClient, buildID string) (int, error) {
	var token []byte
	var pinned []Execution
	for {
		executions, next, err := temporal.ListPinned(ctx, buildID, token)
		if err != nil {
			return 0, fmt.Errorf("worker rollout: list workflows pinned to %q: %w", buildID, err)
		}
		pinned = append(pinned, executions...)
		if len(next) == 0 {
			break
		}
		token = next
	}
	for _, execution := range pinned {
		if err := temporal.SetAutoUpgrade(ctx, execution); err != nil {
			return 0, fmt.Errorf("worker rollout: set %s/%s AUTO_UPGRADE: %w", execution.WorkflowID, execution.RunID, err)
		}
	}
	return len(pinned), nil
}

// SDKClient adapts a Temporal SDK client.
type SDKClient struct {
	Client client.Client
}

// DescribeRouting returns the managed deployment's routing and version lifecycle.
func (s SDKClient) DescribeRouting(ctx context.Context) (workerrouting.Routing, error) {
	return workerrouting.DescribeRouting(ctx, s.Client)
}

// ListPinned returns one visibility page of running workflows pinned to buildID.
func (s SDKClient) ListPinned(ctx context.Context, buildID string, token []byte) ([]Execution, []byte, error) {
	legacy := escapeVisibility(workerrouting.DeploymentName + "." + buildID)
	current := escapeVisibility(workerrouting.DeploymentName + ":" + buildID)
	query := fmt.Sprintf(
		"TemporalWorkerDeploymentVersion IN ('%s', '%s') AND "+
			"TemporalWorkflowVersioningBehavior = 'Pinned' AND ExecutionStatus = 'Running'",
		legacy, current,
	)
	response, err := s.Client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Query:         query,
		NextPageToken: token,
	})
	if err != nil {
		return nil, nil, err
	}
	executions := make([]Execution, 0, len(response.Executions))
	for _, info := range response.Executions {
		execution := info.GetExecution()
		if execution == nil || execution.GetWorkflowId() == "" {
			continue
		}
		executions = append(executions, Execution{
			WorkflowID: execution.GetWorkflowId(),
			RunID:      execution.GetRunId(),
		})
	}
	return executions, response.NextPageToken, nil
}

// SetAutoUpgrade moves an execution to the current build on its next workflow task.
func (s SDKClient) SetAutoUpgrade(ctx context.Context, execution Execution) error {
	_, err := s.Client.UpdateWorkflowExecutionOptions(ctx, client.UpdateWorkflowExecutionOptionsRequest{
		WorkflowId: execution.WorkflowID,
		RunId:      execution.RunID,
		WorkflowExecutionOptionsChanges: client.WorkflowExecutionOptionsChanges{
			VersioningOverride: &client.VersioningOverrideChange{
				Value: &client.AutoUpgradeVersioningOverride{},
			},
		},
	})
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return nil
	}
	return err
}

func escapeVisibility(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}
