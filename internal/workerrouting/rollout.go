package workerrouting

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// MaxRolloutVersions bounds serial RPC work and output for an operator snapshot.
const MaxRolloutVersions = 128

// RolloutReader deliberately excludes every routing and metadata mutation.
type RolloutReader interface {
	Describe(context.Context, client.WorkerDeploymentDescribeOptions) (client.WorkerDeploymentDescribeResponse, error)
	DescribeVersion(context.Context, client.WorkerDeploymentDescribeVersionOptions) (client.WorkerDeploymentVersionDescription, error)
}

// RolloutSnapshot records native routing and observed per-version drainage.
// It does not infer last-known-good, readiness, or permission to retire workers.
type RolloutSnapshot struct {
	Deployment     string           `json:"deployment"`
	Current        string           `json:"current"`
	Ramping        string           `json:"ramping,omitempty"`
	RampPercentage float32          `json:"rampPercentage"`
	Phase          string           `json:"phase"`
	CheckedAt      time.Time        `json:"checkedAt"`
	Versions       []RolloutVersion `json:"versions"`
	Limitation     string           `json:"limitation"`
}

// RolloutVersion keeps the service's drainage timestamp visible. Even drained
// versions may own accepted starts or be needed for queries to closed histories.
type RolloutVersion struct {
	BuildID           string    `json:"buildId"`
	Drainage          string    `json:"drainage"`
	DrainageCheckedAt time.Time `json:"drainageCheckedAt"`
	RoutingChangedAt  time.Time `json:"routingChangedAt"`
}

// ReadRollout reuses the recovered #4084 status/DescribeVersion approach while
// refusing its inferred LKG and retirement claims. The before/after native CAS
// token check prevents returning a mixed routing snapshot during a rollout.
func ReadRollout(ctx context.Context, reader RolloutReader, now time.Time) (RolloutSnapshot, error) {
	before, err := reader.Describe(ctx, client.WorkerDeploymentDescribeOptions{})
	if err != nil {
		return RolloutSnapshot{}, fmt.Errorf("describe rollout: %w", err)
	}
	if before.Info.Name != DeploymentName || len(before.ConflictToken) == 0 {
		return RolloutSnapshot{}, errors.New("rollout deployment identity or conflict token unavailable")
	}
	if len(before.Info.VersionSummaries) > MaxRolloutVersions {
		return RolloutSnapshot{}, fmt.Errorf("rollout exceeds %d-version inspection bound", MaxRolloutVersions)
	}
	routing := before.Info.RoutingConfig
	percentage := routing.RampingVersionPercentage
	if math.IsNaN(float64(percentage)) || math.IsInf(float64(percentage), 0) || percentage < 0 || percentage > 100 {
		return RolloutSnapshot{}, errors.New("rollout percentage is invalid")
	}
	current, err := rolloutTarget(routing.CurrentVersion)
	if err != nil {
		return RolloutSnapshot{}, err
	}
	result := RolloutSnapshot{Deployment: DeploymentName, Current: current, RampPercentage: percentage,
		CheckedAt: now.UTC(), Phase: "stable", Versions: []RolloutVersion{},
		Limitation: "read-only routing evidence; no readiness, compatibility, last-known-good or retirement guarantee; pinned histories and accepted starts retain their build"}
	if routing.RampingVersion != nil || percentage > 0 {
		result.Ramping, err = rolloutTarget(routing.RampingVersion)
		if err != nil {
			return RolloutSnapshot{}, err
		}
		if result.Ramping == result.Current {
			return RolloutSnapshot{}, errors.New("current and ramping targets are identical")
		}
		result.Phase = "ramping"
		if percentage == 0 {
			result.Phase = "paused"
		}
	}
	result.Versions, err = readRolloutVersions(ctx, reader, before.Info.VersionSummaries, routing)
	if err != nil {
		return RolloutSnapshot{}, err
	}
	after, err := reader.Describe(ctx, client.WorkerDeploymentDescribeOptions{})
	if err != nil {
		return RolloutSnapshot{}, fmt.Errorf("recheck rollout: %w", err)
	}
	if !bytes.Equal(before.ConflictToken, after.ConflictToken) {
		return RolloutSnapshot{}, errors.New("rollout changed during inspection; retry for a coherent snapshot")
	}
	sort.Slice(result.Versions, func(i, j int) bool { return result.Versions[i].BuildID < result.Versions[j].BuildID })
	return result, nil
}

func readRolloutVersions(ctx context.Context, reader RolloutReader, summaries []client.WorkerDeploymentVersionSummary, routing client.WorkerDeploymentRoutingConfig) ([]RolloutVersion, error) {
	versions := []RolloutVersion{}
	seen := map[string]bool{}
	for _, summary := range summaries {
		if summary.Version.DeploymentName != DeploymentName || summary.Version.BuildID == "" || seen[summary.Version.BuildID] {
			return nil, errors.New("rollout version inventory is inconsistent")
		}
		seen[summary.Version.BuildID] = true
		version, err := reader.DescribeVersion(ctx, client.WorkerDeploymentDescribeVersionOptions{BuildID: summary.Version.BuildID})
		if err != nil {
			return nil, fmt.Errorf("describe version %q: %w", summary.Version.BuildID, err)
		}
		if version.Info.Version != summary.Version {
			return nil, errors.New("rollout version identity differs")
		}
		row := RolloutVersion{BuildID: summary.Version.BuildID, Drainage: "unknown", RoutingChangedAt: version.Info.RoutingChangedTime}
		if drainage := version.Info.DrainageInfo; drainage != nil {
			row.DrainageCheckedAt = drainage.LastCheckedTime
			switch drainage.DrainageStatus {
			case client.WorkerDeploymentVersionDrainageStatusDraining:
				row.Drainage = "draining"
			case client.WorkerDeploymentVersionDrainageStatusDrained:
				row.Drainage = "drained"
			}
		}
		versions = append(versions, row)
	}
	for _, target := range []*worker.WorkerDeploymentVersion{routing.CurrentVersion, routing.RampingVersion} {
		if target != nil && !seen[target.BuildID] {
			return nil, errors.New("routed build absent from version inventory")
		}
	}
	return versions, nil
}

func rolloutTarget(version *worker.WorkerDeploymentVersion) (string, error) {
	if version == nil {
		return UnversionedLabel, nil
	}
	if version.DeploymentName != DeploymentName || version.BuildID == "" {
		return "", errors.New("rollout target identity is invalid")
	}
	return version.DeploymentName + "." + version.BuildID, nil
}
