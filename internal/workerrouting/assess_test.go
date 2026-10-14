package workerrouting

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeDeployment is a Temporal worker deployment plus the task queue's
// pollers, served without a server.
type fakeDeployment struct {
	routing     client.WorkerDeploymentRoutingConfig
	describeErr error
	pollers     []*taskqueuepb.PollerInfo
	pollersErr  error
	queues      []string
}

func (f *fakeDeployment) DescribeRouting(context.Context) (client.WorkerDeploymentRoutingConfig, error) {
	return f.routing, f.describeErr
}

func (f *fakeDeployment) WorkflowPollers(_ context.Context, taskQueue string) ([]*taskqueuepb.PollerInfo, error) {
	f.queues = append(f.queues, taskQueue)
	return f.pollers, f.pollersErr
}

var assessNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func versionedPoller(build string, age time.Duration) *taskqueuepb.PollerInfo {
	return &taskqueuepb.PollerInfo{
		Identity:       "goobers-worker/" + build,
		LastAccessTime: timestamppb.New(assessNow.Add(-age)),
		DeploymentOptions: &deploymentpb.WorkerDeploymentOptions{
			DeploymentName:       DeploymentName,
			BuildId:              build,
			WorkerVersioningMode: enumspb.WORKER_VERSIONING_MODE_VERSIONED,
		},
	}
}

func unversionedPoller(age time.Duration) *taskqueuepb.PollerInfo {
	return &taskqueuepb.PollerInfo{Identity: "goobers-worker/plain", LastAccessTime: timestamppb.New(assessNow.Add(-age))}
}

func version(build string) *worker.WorkerDeploymentVersion {
	return &worker.WorkerDeploymentVersion{DeploymentName: DeploymentName, BuildID: build}
}

func TestAssess(t *testing.T) {
	cases := []struct {
		name        string
		fake        fakeDeployment
		state       State
		detail      string
		remediation []string
		workers     []WorkerRouting
	}{
		{
			// The #5950/#6020 production shape: a Ready versioned worker whose
			// build was never made current polls successfully and gets nothing.
			name:  "versioned build never made current stalls",
			fake:  fakeDeployment{pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second), versionedPoller("v2", 3*time.Second)}},
			state: StateStalled, detail: "current version is __unversioned__, but the live workers serve goobers.v2",
			remediation: []string{"temporal worker deployment set-current-version --deployment-name goobers --build-id v2"},
			workers:     []WorkerRouting{{Version: "goobers.v2", Pollers: 2, Routing: WorkerUnrouted}},
		},
		{
			name: "previous build still current after its workers left",
			fake: fakeDeployment{
				routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: version("v1")},
				// The departed v1 worker is still listed by the server, but
				// stale: it must not count as serving.
				pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second), versionedPoller("v1", PollerMaxAge+time.Second)},
			},
			state: StateStalled, detail: "current version is goobers.v1",
			remediation: []string{"temporal worker deployment set-current-version --deployment-name goobers --build-id v2"},
			workers:     []WorkerRouting{{Version: "goobers.v2", Pollers: 1, Routing: WorkerUnrouted}},
		},
		{
			name: "rollout in progress: new build unrouted while the old one serves",
			fake: fakeDeployment{
				routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: version("v1")},
				pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second), versionedPoller("v1", time.Second)},
			},
			state: StateDegraded, detail: "goobers.v1 still served",
			remediation: []string{"temporal worker deployment set-current-version --deployment-name goobers --build-id v2"},
			workers: []WorkerRouting{
				{Version: "goobers.v1", Pollers: 1, Routing: WorkerRouted},
				{Version: "goobers.v2", Pollers: 1, Routing: WorkerUnrouted},
			},
		},
		{
			name: "versioned build current",
			fake: fakeDeployment{
				routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: version("v2")},
				pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second)},
			},
			state: StateHealthy, detail: "current version goobers.v2",
			workers: []WorkerRouting{{Version: "goobers.v2", Pollers: 1, Routing: WorkerRouted}},
		},
		{
			name: "versioned build ramping",
			fake: fakeDeployment{
				routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: version("v1"), RampingVersion: version("v2"), RampingVersionPercentage: 25},
				pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second)},
			},
			state: StateHealthy, detail: "routes workflow tasks",
			workers: []WorkerRouting{{Version: "goobers.v2", Pollers: 1, Routing: WorkerRouted}},
		},
		{
			name:   "unversioned workers with no deployment",
			fake:   fakeDeployment{describeErr: serviceerror.NewNotFound("no Worker Deployment found"), pollers: []*taskqueuepb.PollerInfo{unversionedPoller(time.Second)}},
			state:  StateHealthy,
			detail: "no versioned worker has registered the deployment",
			workers: []WorkerRouting{
				{Version: UnversionedLabel, Pollers: 1, Routing: WorkerRouted},
			},
		},
		{
			// An instance that ran versioned, then upgraded to the unversioned
			// default: the deployment still routes to its old version.
			name: "unversioned workers but deployment still routes to a version",
			fake: fakeDeployment{
				routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: version("dev")},
				pollers: []*taskqueuepb.PollerInfo{unversionedPoller(time.Second)},
			},
			state: StateStalled, detail: "engine.workerVersioning is off",
			remediation: []string{"temporal worker deployment set-current-version --deployment-name goobers --unversioned"},
			workers:     []WorkerRouting{{Version: UnversionedLabel, Pollers: 1, Routing: WorkerUnrouted}},
		},
		{
			name:   "task queue unreadable",
			fake:   fakeDeployment{pollersErr: errors.New("connection refused")},
			state:  StateUnknown,
			detail: "cannot list the workers polling task queue \"goobers-engine\": connection refused",
		},
		{
			name:   "no live worker",
			fake:   fakeDeployment{pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", PollerMaxAge+time.Minute)}},
			state:  StateUnknown,
			detail: "no goobers worker has polled task queue \"goobers-engine\"",
		},
		{
			name:   "deployment unreadable",
			fake:   fakeDeployment{describeErr: serviceerror.NewPermissionDenied("denied", ""), pollers: []*taskqueuepb.PollerInfo{unversionedPoller(time.Second)}},
			state:  StateUnknown,
			detail: "cannot read worker deployment \"goobers\" routing",
		},
		{
			name:   "versioned workers but deployment missing",
			fake:   fakeDeployment{describeErr: serviceerror.NewNotFound("gone"), pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second)}},
			state:  StateUnknown,
			detail: "cannot be described",
			workers: []WorkerRouting{
				{Version: "goobers.v2", Pollers: 1, Routing: WorkerUnknown},
			},
		},
		{
			name: "pollers of another deployment are not goobers workers",
			fake: fakeDeployment{pollers: []*taskqueuepb.PollerInfo{{
				LastAccessTime: timestamppb.New(assessNow),
				DeploymentOptions: &deploymentpb.WorkerDeploymentOptions{
					DeploymentName: "other", BuildId: "x", WorkerVersioningMode: enumspb.WORKER_VERSIONING_MODE_VERSIONED,
				},
			}}},
			state:  StateUnknown,
			detail: "no goobers worker has polled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake
			got := Assess(context.Background(), &fake, "goobers-engine", assessNow)
			if got.State != tc.state {
				t.Fatalf("state = %s, want %s (detail %q)", got.State, tc.state, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("detail = %q, want it to contain %q", got.Detail, tc.detail)
			}
			if strings.Join(got.Remediation, "|") != strings.Join(tc.remediation, "|") {
				t.Fatalf("remediation = %q, want %q", got.Remediation, tc.remediation)
			}
			if len(got.Workers) != len(tc.workers) {
				t.Fatalf("workers = %+v, want %+v", got.Workers, tc.workers)
			}
			for i := range tc.workers {
				if got.Workers[i] != tc.workers[i] {
					t.Fatalf("workers = %+v, want %+v", got.Workers, tc.workers)
				}
			}
			if got.TaskQueue != "goobers-engine" || got.Deployment != DeploymentName || !got.CheckedAt.Equal(assessNow) {
				t.Fatalf("identity = %+v", got)
			}
			if len(fake.queues) != 1 || fake.queues[0] != "goobers-engine" {
				t.Fatalf("polled queues = %q", fake.queues)
			}
		})
	}
}

func TestStatusLine(t *testing.T) {
	stalled := Assess(context.Background(), &fakeDeployment{pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second)}}, "q", assessNow)
	line := stalled.StatusLine()
	for _, want := range []string{"Warning: engine routing stalled:", "Run: temporal worker deployment set-current-version --deployment-name goobers --build-id v2"} {
		if !strings.Contains(line, want) {
			t.Fatalf("status line %q lacks %q", line, want)
		}
	}
	unknown := Unknown("q", "cannot dial", assessNow).StatusLine()
	if unknown != "Engine routing: unknown (cannot dial)\n" {
		t.Fatalf("unknown status line = %q", unknown)
	}
	healthy := Assess(context.Background(), &fakeDeployment{
		routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: version("v2")},
		pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second)},
	}, "q", assessNow)
	if line := healthy.StatusLine(); line != "" {
		t.Fatalf("healthy status line = %q, want silence", line)
	}
}

func TestAssessmentJSONShape(t *testing.T) {
	a := Assess(context.Background(), &fakeDeployment{pollers: []*taskqueuepb.PollerInfo{versionedPoller("v2", time.Second)}}, "goobers-engine", assessNow)
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"state":"stalled"`, `"deployment":"goobers"`, `"taskQueue":"goobers-engine"`, `"currentVersion":"__unversioned__"`,
		`"workers":[{"version":"goobers.v2","pollers":1,"routing":"unrouted"}]`,
		`"remediation":["temporal worker deployment set-current-version --deployment-name goobers --build-id v2"]`,
		`"checkedAt":"2026-10-14T12:00:00Z"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("JSON %s lacks %s", raw, want)
		}
	}
}

func TestCheckCommands(t *testing.T) {
	if got := Check(true, "v2", client.WorkerDeploymentRoutingConfig{}, nil); got.Command != BuildCommand("v2") || !strings.Contains(got.Problem, got.Command) {
		t.Fatalf("versioned finding = %+v", got)
	}
	if got := Check(false, "v2", client.WorkerDeploymentRoutingConfig{CurrentVersion: version("v1")}, nil); got.Command != UnversionedCommand() || !strings.Contains(got.Problem, got.Command) {
		t.Fatalf("unversioned finding = %+v", got)
	}
	if got := Check(true, "v2", client.WorkerDeploymentRoutingConfig{}, errors.New("boom")); !got.Unverified || got.Command != "" || got.Routed() {
		t.Fatalf("unverified finding = %+v", got)
	}
}
