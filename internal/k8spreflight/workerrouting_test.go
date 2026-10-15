package k8spreflight

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workerrouting"
)

// fakeRoutingTemporal is a dialed Temporal frontend that also serves the
// worker deployment's routing and the engine queue's pollers.
type fakeRoutingTemporal struct {
	fakeTemporalDescriber
	routing     client.WorkerDeploymentRoutingConfig
	describeErr error
	pollers     []*taskqueuepb.PollerInfo
	queues      []string
}

func (f *fakeRoutingTemporal) DescribeRouting(context.Context) (workerrouting.Routing, error) {
	return workerrouting.Routing{WorkerDeploymentRoutingConfig: f.routing}, f.describeErr
}

func (f *fakeRoutingTemporal) WorkflowPollers(_ context.Context, taskQueue string) ([]*taskqueuepb.PollerInfo, error) {
	f.queues = append(f.queues, taskQueue)
	return f.pollers, nil
}

func routingPoller(build string) *taskqueuepb.PollerInfo {
	return &taskqueuepb.PollerInfo{
		LastAccessTime: timestamppb.New(time.Now()),
		DeploymentOptions: &deploymentpb.WorkerDeploymentOptions{
			DeploymentName:       workerrouting.DeploymentName,
			BuildId:              build,
			WorkerVersioningMode: enumspb.WORKER_VERSIONING_MODE_VERSIONED,
		},
	}
}

func healthyRoutingTemporal(namespace string) *fakeRoutingTemporal {
	return &fakeRoutingTemporal{
		fakeTemporalDescriber: fakeTemporalDescriber{wantNamespace: namespace},
		routing:               client.WorkerDeploymentRoutingConfig{CurrentVersion: &worker.WorkerDeploymentVersion{DeploymentName: "goobers", BuildID: "v2"}},
		pollers:               []*taskqueuepb.PollerInfo{routingPoller("v2")},
	}
}

func runWorkerRouting(t *testing.T, opts Options) Result {
	t.Helper()
	opts.Checks = []string{"temporal-worker-routing"}
	return resultByID(t, Run(context.Background(), nil, opts), "temporal-worker-routing")
}

func TestTemporalWorkerRouting(t *testing.T) {
	stalled := &fakeRoutingTemporal{pollers: []*taskqueuepb.PollerInfo{routingPoller("v2")}}
	rollout := &fakeRoutingTemporal{
		routing: client.WorkerDeploymentRoutingConfig{CurrentVersion: &worker.WorkerDeploymentVersion{DeploymentName: "goobers", BuildID: "v1"}},
		pollers: []*taskqueuepb.PollerInfo{routingPoller("v1"), routingPoller("v2")},
	}
	unreadable := &fakeRoutingTemporal{describeErr: errors.New("permission denied"), pollers: []*taskqueuepb.PollerInfo{routingPoller("v2")}}
	cases := []struct {
		name     string
		dialed   temporalNamespaceDescriber
		dialErr  error
		status   Status
		detail   string
		hint     string
		closeErr bool
	}{
		{name: "healthy", dialed: healthyRoutingTemporal(""), status: StatusPass, detail: "healthy: worker deployment"},
		{
			name: "stalled", dialed: stalled, status: StatusFail, detail: "stalled: worker deployment",
			hint: "run: temporal worker deployment set-current-version --deployment-name goobers --build-id v2",
		},
		{
			name: "rollout not promoted", dialed: rollout, status: StatusWarn, detail: "degraded:",
			hint: "run: temporal worker deployment set-current-version --deployment-name goobers --build-id v2",
		},
		{name: "routing unreadable", dialed: unreadable, status: StatusWarn, detail: "unknown: cannot read worker deployment"},
		{name: "frontend unreachable", dialErr: errors.New("connection refused"), status: StatusWarn, detail: "unknown: could not reach Temporal frontend"},
		{name: "client without routing reads", dialed: &fakeTemporalDescriber{}, status: StatusWarn, detail: "unknown: this Temporal client cannot read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := runWorkerRouting(t, Options{
				TemporalHostPort:  "temporal-frontend.goobers-temporal:7233",
				TemporalTaskQueue: "engine-q",
				DialTemporal: func(context.Context, string) (temporalNamespaceDescriber, error) {
					if tc.dialErr != nil {
						return nil, tc.dialErr
					}
					return tc.dialed, nil
				},
			})
			if result.Status != tc.status || result.Severity != SeverityRequired {
				t.Fatalf("result = %s/%s, want %s/required (detail %q)", result.Status, result.Severity, tc.status, result.Detail)
			}
			if !strings.HasPrefix(result.Detail, tc.detail) {
				t.Fatalf("detail = %q, want prefix %q", result.Detail, tc.detail)
			}
			if result.Hint != tc.hint {
				t.Fatalf("hint = %q, want %q", result.Hint, tc.hint)
			}
			if fake, ok := tc.dialed.(*fakeRoutingTemporal); ok {
				if len(fake.queues) != 1 || fake.queues[0] != "engine-q" {
					t.Fatalf("assessed queues = %q, want the configured engine queue", fake.queues)
				}
				if !fake.closed {
					t.Fatal("the Temporal client was never closed")
				}
			}
		})
	}
}

func TestTemporalWorkerRoutingStalledIsNonConformant(t *testing.T) {
	report := Run(context.Background(), nil, Options{
		Checks:           []string{"temporal-worker-routing"},
		TemporalHostPort: "temporal:7233",
		DialTemporal: func(context.Context, string) (temporalNamespaceDescriber, error) {
			return &fakeRoutingTemporal{pollers: []*taskqueuepb.PollerInfo{routingPoller("v2")}}, nil
		},
	})
	if report.Conformant {
		t.Fatalf("a stalled engine must fail the preflight: %+v", report.Results)
	}
}

func TestTemporalWorkerRoutingSkippedWhenUnconfigured(t *testing.T) {
	result := runWorkerRouting(t, Options{})
	if result.Status != StatusWarn || result.Severity != SeverityOptional || !strings.Contains(result.Detail, "skipped") {
		t.Fatalf("result = %+v, want an optional skipped warn", result)
	}
}

// Without --instance, the check reads the default engine queue and says so
// when it finds no worker there.
func TestTemporalWorkerRoutingDefaultsTheEngineQueue(t *testing.T) {
	fake := &fakeRoutingTemporal{}
	result := runWorkerRouting(t, Options{
		TemporalHostPort: "temporal:7233",
		DialTemporal: func(context.Context, string) (temporalNamespaceDescriber, error) {
			return fake, nil
		},
	})
	if len(fake.queues) != 1 || fake.queues[0] != "goobers-engine" {
		t.Fatalf("assessed queues = %q, want goobers-engine", fake.queues)
	}
	if result.Status != StatusWarn || !strings.Contains(result.Hint, "--instance") {
		t.Fatalf("result = %+v, want unknown warn pointing at --instance", result)
	}
}

func TestResolveTemporalOptionsReadsEngineTaskQueue(t *testing.T) {
	root := t.TempDir()
	cfg := &instance.Config{Engine: &instance.EngineConfig{HostPort: "temporal:7233", TaskQueue: "custom-engine"}}
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	opts, err := ResolveTemporalOptions(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.TemporalTaskQueue != "custom-engine" {
		t.Fatalf("TemporalTaskQueue = %q, want the instance engine.taskQueue", opts.TemporalTaskQueue)
	}
	opts, err = ResolveTemporalOptions(root, Options{TemporalTaskQueue: "explicit"})
	if err != nil || opts.TemporalTaskQueue != "explicit" {
		t.Fatalf("explicit queue = %q, %v; want it kept", opts.TemporalTaskQueue, err)
	}
}
