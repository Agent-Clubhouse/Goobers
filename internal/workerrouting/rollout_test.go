package workerrouting

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

type rolloutFake struct {
	description client.WorkerDeploymentDescribeResponse
	versions    map[string]client.WorkerDeploymentVersionDescription
	reads       int
	changed     bool
	versionErr  error
}

func (f *rolloutFake) Describe(context.Context, client.WorkerDeploymentDescribeOptions) (client.WorkerDeploymentDescribeResponse, error) {
	f.reads++
	result := f.description
	if f.changed && f.reads > 1 {
		result.ConflictToken = []byte("new")
	}
	return result, nil
}

func (f *rolloutFake) DescribeVersion(_ context.Context, opts client.WorkerDeploymentDescribeVersionOptions) (client.WorkerDeploymentVersionDescription, error) {
	return f.versions[opts.BuildID], f.versionErr
}

func rolloutFixture() *rolloutFake {
	a := worker.WorkerDeploymentVersion{DeploymentName: DeploymentName, BuildID: "a"}
	b := worker.WorkerDeploymentVersion{DeploymentName: DeploymentName, BuildID: "b"}
	return &rolloutFake{
		description: client.WorkerDeploymentDescribeResponse{ConflictToken: []byte("original"), Info: client.WorkerDeploymentInfo{Name: DeploymentName,
			RoutingConfig:    client.WorkerDeploymentRoutingConfig{CurrentVersion: &b, RampingVersion: &a},
			VersionSummaries: []client.WorkerDeploymentVersionSummary{{Version: b}, {Version: a}}}},
		versions: map[string]client.WorkerDeploymentVersionDescription{
			"a": {Info: client.WorkerDeploymentVersionInfo{Version: a}},
			"b": {Info: client.WorkerDeploymentVersionInfo{Version: b}},
		},
	}
}

func TestReadRolloutPreservesPausedAndNativeDrainEvidence(t *testing.T) {
	f := rolloutFixture()
	got, err := ReadRollout(context.Background(), f, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "paused" || got.Ramping != "goobers.a" || got.Versions[0].BuildID != "a" || got.Versions[0].Drainage != "unknown" {
		t.Fatalf("snapshot = %+v", got)
	}
	// A stopped ramp is still referenced; it is not a drained version.
	f.description.Info.RoutingConfig.RampingVersion = nil
	checked := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	version := f.versions["a"]
	version.Info.DrainageInfo = &client.WorkerDeploymentVersionDrainageInfo{DrainageStatus: client.WorkerDeploymentVersionDrainageStatusDrained, LastCheckedTime: checked}
	f.versions["a"] = version
	got, err = ReadRollout(context.Background(), f, checked.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Versions[0].Drainage != "drained" || got.Versions[0].DrainageCheckedAt != checked || !strings.Contains(got.Limitation, "no readiness") {
		t.Fatalf("lost native evidence/limit: %+v", got)
	}
}

func TestReadRolloutFailsClosedOnIncompleteOrChangedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*rolloutFake)
	}{
		{"changed", func(f *rolloutFake) { f.changed = true }},
		{"version-read-fails", func(f *rolloutFake) { f.versionErr = errors.New("unavailable") }},
		{"missing-route-inventory", func(f *rolloutFake) { f.description.Info.VersionSummaries = nil }},
		{"missing-token", func(f *rolloutFake) { f.description.ConflictToken = nil }},
		{"wrong-deployment", func(f *rolloutFake) { f.description.Info.Name = "other" }},
		{"too-large", func(f *rolloutFake) {
			f.description.Info.VersionSummaries = make([]client.WorkerDeploymentVersionSummary, MaxRolloutVersions+1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := rolloutFixture()
			tc.change(f)
			got, err := ReadRollout(context.Background(), f, time.Now())
			if err == nil || got.Deployment != "" {
				t.Fatalf("partial snapshot escaped: %+v, %v", got, err)
			}
		})
	}
}

func TestReadRolloutRecognizesRampingToUnversioned(t *testing.T) {
	f := rolloutFixture()
	f.description.Info.RoutingConfig.RampingVersion = nil
	f.description.Info.RoutingConfig.RampingVersionPercentage = 25
	got, err := ReadRollout(context.Background(), f, time.Now())
	if err != nil || got.Phase != "ramping" || got.Ramping != UnversionedLabel || got.RampPercentage != 25 {
		t.Fatalf("snapshot = %+v, %v", got, err)
	}
}
