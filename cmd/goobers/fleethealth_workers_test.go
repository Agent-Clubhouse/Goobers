package main

import (
	"context"
	"errors"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/goobers/goobers/internal/diagnostics/fleetstate"
	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

type fleetWorkerDescriber struct {
	responses map[enumspb.TaskQueueType]*workflowservice.DescribeTaskQueueResponse
	err       error
	block     bool
	calls     int
}

func (f *fleetWorkerDescriber) DescribeTaskQueue(ctx context.Context, queue string, kind enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error) {
	f.calls++
	if queue != "engine-queue" {
		return nil, errors.New("unexpected queue")
	}
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.responses[kind], f.err
}
func fleetPollers(at ...time.Time) *workflowservice.DescribeTaskQueueResponse {
	r := &workflowservice.DescribeTaskQueueResponse{}
	for _, t := range at {
		r.Pollers = append(r.Pollers, &taskqueuepb.PollerInfo{LastAccessTime: timestamppb.New(t)})
	}
	return r
}
func workerObserver(client *fleetWorkerDescriber, drivers map[localscheduler.WorkflowIdentity]string) *fleetWorkerHealthObserver {
	return &fleetWorkerHealthObserver{client: client, queue: "engine-queue", drivers: func() map[localscheduler.WorkflowIdentity]string { return drivers }}
}
func TestFleetWorkerHealthEvidence(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, state string
		response    *workflowservice.DescribeTaskQueueResponse
		err         error
	}{
		{"absent", "no_recent_poller", fleetPollers(), nil},
		{"present", "recent_poller", fleetPollers(now.Add(-time.Second)), nil},
		{"expired", "no_recent_poller", fleetPollers(now.Add(-fleetWorkerPollerMaxAge - time.Second)), nil},
		{"poll during RPC", "recent_poller", fleetPollers(now.Add(time.Second)), nil},
		{"future", "unknown", fleetPollers(now.Add(fleetWorkerClockTolerance + time.Second)), nil},
		{"timestamp missing", "unknown", &workflowservice.DescribeTaskQueueResponse{Pollers: []*taskqueuepb.PollerInfo{{}}}, nil},
		{"nil response", "unknown", nil, nil},
		{"unavailable", "unknown", fleetPollers(now), errors.New("private frontend detail")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fleetWorkerDescriber{responses: map[enumspb.TaskQueueType]*workflowservice.DescribeTaskQueueResponse{enumspb.TASK_QUEUE_TYPE_WORKFLOW: tc.response, enumspb.TASK_QUEUE_TYPE_ACTIVITY: tc.response}, err: tc.err}
			o := workerObserver(client, map[localscheduler.WorkflowIdentity]string{{Gaggle: "engine", Workflow: "work"}: "runner.engine"})
			o.beginPulse(context.Background(), now)
			obs := fleetstate.Observation{Complete: true}
			attrs := map[string]any{}
			o.observe("engine", &obs, attrs)
			if attrs["workerObservation"] != tc.state {
				t.Fatal(attrs)
			}
			if tc.state == "unknown" && (obs.Complete || obs.MissingWorkerCount != nil || attrs["missingWorkerCount"] != nil) {
				t.Fatalf("unknown fabricated completeness: %+v %v", obs, attrs)
			}
			if tc.state == "no_recent_poller" && (obs.MissingWorkerCount == nil || *obs.MissingWorkerCount != 1) {
				t.Fatalf("missing not reported: %+v", obs)
			}
		})
	}
}
func TestFleetWorkerHealthMixedScopeAndRecovery(t *testing.T) {
	now := time.Now().UTC()
	client := &fleetWorkerDescriber{responses: map[enumspb.TaskQueueType]*workflowservice.DescribeTaskQueueResponse{enumspb.TASK_QUEUE_TYPE_WORKFLOW: fleetPollers(), enumspb.TASK_QUEUE_TYPE_ACTIVITY: fleetPollers(now)}}
	drivers := map[localscheduler.WorkflowIdentity]string{{Gaggle: "engine", Workflow: "work"}: "runner.engine", {Gaggle: "engine", Workflow: "other"}: "runner.local", {Gaggle: "local", Workflow: "work"}: "runner.local"}
	o := workerObserver(client, drivers)
	o.beginPulse(context.Background(), now)
	for _, gaggle := range []string{"engine", "local", "unknown"} {
		obs := fleetstate.Observation{Complete: true}
		attrs := map[string]any{}
		o.observe(gaggle, &obs, attrs)
		if gaggle == "engine" && attrs["workerObservation"] != "no_recent_poller" {
			t.Fatal(attrs)
		}
		if gaggle == "local" && (attrs["workerObservation"] != "not_required" || !obs.Complete || obs.MissingWorkerCount != nil) {
			t.Fatal(attrs)
		}
		if gaggle == "unknown" && (attrs["workerObservation"] != "unknown" || obs.Complete) {
			t.Fatal(attrs)
		}
	}
	if client.calls != 2 {
		t.Fatalf("per-gaggle RPC fanout: %d", client.calls)
	}
	client.err = errors.New("unavailable")
	o.beginPulse(context.Background(), now.Add(time.Second))
	if o.state != "unknown" {
		t.Fatal("stale missing evidence latched", o.state)
	}
	client.err = nil
	client.responses[enumspb.TASK_QUEUE_TYPE_WORKFLOW] = fleetPollers(now)
	o.beginPulse(context.Background(), now.Add(2*time.Second))
	if o.state != "recent_poller" {
		t.Fatal("recovery failed", o.state)
	}
	delete(drivers, localscheduler.WorkflowIdentity{Gaggle: "engine", Workflow: "work"})
	calls := client.calls
	o.beginPulse(context.Background(), now.Add(3*time.Second))
	if client.calls != calls {
		t.Fatal("local-only reload queried engine")
	}
}
func TestFleetWorkerHealthBounds(t *testing.T) {
	now := time.Now().UTC()
	client := &fleetWorkerDescriber{block: true}
	drivers := map[localscheduler.WorkflowIdentity]string{{Gaggle: "engine", Workflow: "work"}: "runner.engine"}
	o := workerObserver(client, drivers)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	o.beginPulse(ctx, now)
	if time.Since(start) > time.Second || o.state != "unknown" || client.calls != 1 {
		t.Fatalf("unbounded unavailable observation: %v %d", o.state, client.calls)
	}
	client.block = false
	client.calls = 0
	o.drivers = func() map[localscheduler.WorkflowIdentity]string { return nil }
	o.beginPulse(context.Background(), now)
	if client.calls != 0 || o.gaggles != nil {
		t.Fatal("unknown admissions queried engine")
	}
	oversized := &workflowservice.DescribeTaskQueueResponse{Pollers: make([]*taskqueuepb.PollerInfo, fleetWorkerDefinitionLimit+1)}
	if fleetPollerState(oversized, now) != "unknown" {
		t.Fatal("unbounded poller inventory reported complete")
	}
}
func TestFleetWorkerHealthProductionSampler(t *testing.T) {
	now := time.Now().UTC()
	reader := &fleetTestReader{now: now, eligible: true}
	client := &fleetWorkerDescriber{responses: map[enumspb.TaskQueueType]*workflowservice.DescribeTaskQueueResponse{enumspb.TASK_QUEUE_TYPE_WORKFLOW: fleetPollers(), enumspb.TASK_QUEUE_TYPE_ACTIVITY: fleetPollers()}}
	workers := workerObserver(client, map[localscheduler.WorkflowIdentity]string{{Gaggle: "alpha", Workflow: "work"}: "runner.engine"})
	sample := newFleetHealthSampler(t.TempDir(), &daemonIdentity{StartedAt: now.Add(-time.Minute)}, &instance.DiagnosticsConfig{}, reader, workers)
	attrs := sample(context.Background(), now)[1].Attributes
	attrs["instanceId"], attrs["deploymentId"] = "test-instance", "test-deployment"
	if _, err := fleetdiagnostics.DecodeHeartbeat(attrs); err != nil {
		t.Fatalf("production worker heartbeat rejected: %v", err)
	}
	if attrs["state"] != "waiting" || attrs["reasonCode"] != "worker_unavailable" || attrs["missingWorkerCount"] != 1 {
		t.Fatal(attrs)
	}
	client.responses[enumspb.TASK_QUEUE_TYPE_WORKFLOW] = fleetPollers(now)
	client.responses[enumspb.TASK_QUEUE_TYPE_ACTIVITY] = fleetPollers(now)
	attrs = sample(context.Background(), now)[1].Attributes
	if attrs["reasonCode"] == "worker_unavailable" || attrs["missingWorkerCount"] != 0 {
		t.Fatal("recovered evidence not applied", attrs)
	}
}

func TestFleetWorkerHealthProductionAdmissionSnapshot(t *testing.T) {
	identity := localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "work"}
	registry := newInterventionDefinitionRegistry(interventionDefinitionSet{featureDrivers: map[localscheduler.WorkflowIdentity]string{identity: "runner.local"}})
	setup := &schedulerSetup{Config: &instance.Config{}, Interventions: registry}
	observer := newFleetWorkerHealthObserver(setup, nil)
	observer.beginPulse(context.Background(), time.Now())
	observation := fleetstate.Observation{Complete: true}
	attrs := map[string]any{}
	observer.observe("alpha", &observation, attrs)
	if attrs["workerObservation"] != "not_required" || !observation.Complete {
		t.Fatal(attrs)
	}
	registry.Replace(interventionDefinitionSet{featureDrivers: map[localscheduler.WorkflowIdentity]string{identity: "runner.engine"}})
	observer.beginPulse(context.Background(), time.Now())
	observation = fleetstate.Observation{Complete: true}
	observer.observe("alpha", &observation, attrs)
	if attrs["workerObservation"] != "unknown" || observation.Complete {
		t.Fatal("engine without client must be unknown", attrs)
	}
}
