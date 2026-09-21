package main

import (
	"context"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"

	"github.com/goobers/goobers/internal/diagnostics/fleetstate"
	"github.com/goobers/goobers/internal/localscheduler"
)

const (
	fleetWorkerDefinitionLimit = 1000
	fleetWorkerPulseTimeout    = time.Second
	fleetWorkerRPCTimeout      = 500 * time.Millisecond
	fleetWorkerPollerMaxAge    = 2 * time.Minute
)

// fleetWorkerHealthObserver observes the engine's shared workflow/activity
// queue, not worker process liveness or dispatch pod availability. It runs only
// on the health goroutine and never participates in execution/admission.
type fleetWorkerHealthObserver struct {
	client     engineTaskQueueDescriber
	queue      string
	drivers    func() map[localscheduler.WorkflowIdentity]string
	gaggles    map[string]fleetWorkerScope
	observedAt time.Time
	state      string
}

type fleetWorkerScope struct{ engine, unknown bool }

func (o *fleetWorkerHealthObserver) beginPulse(ctx context.Context, now time.Time) {
	if o == nil {
		return
	}
	o.gaggles, o.observedAt, o.state = nil, now, "unknown"
	if o.drivers == nil {
		return
	}
	drivers := o.drivers()
	if len(drivers) == 0 || len(drivers) > fleetWorkerDefinitionLimit {
		return
	}
	o.gaggles = make(map[string]fleetWorkerScope)
	engine := false
	for identity, driver := range drivers {
		scope := o.gaggles[identity.Gaggle]
		switch driver {
		case "runner.engine":
			scope.engine, engine = true, true
		case "runner.local":
		default:
			scope.unknown = true
		}
		o.gaggles[identity.Gaggle] = scope
	}
	if !engine || o.client == nil || o.queue == "" {
		return
	}
	pulse, cancel := context.WithTimeout(ctx, fleetWorkerPulseTimeout)
	defer cancel()
	o.state = o.probe(pulse, now)
}

func (o *fleetWorkerHealthObserver) probe(ctx context.Context, now time.Time) string {
	missing := false
	for _, kind := range []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_ACTIVITY} {
		call, cancel := context.WithTimeout(ctx, fleetWorkerRPCTimeout)
		response, err := o.client.DescribeTaskQueue(call, o.queue, kind)
		cancel()
		if err != nil {
			return "unknown"
		}
		switch fleetPollerState(response, now) {
		case "unknown":
			return "unknown"
		case "no_recent_poller":
			missing = true
		}
	}
	if missing {
		return "no_recent_poller"
	}
	return "recent_poller"
}

func fleetPollerState(response *workflowservice.DescribeTaskQueueResponse, now time.Time) string {
	if response == nil || len(response.GetPollers()) > fleetWorkerDefinitionLimit {
		return "unknown"
	}
	unknown := false
	for _, poller := range response.GetPollers() {
		at := poller.GetLastAccessTime()
		if at == nil || at.CheckValid() != nil || at.AsTime().IsZero() || at.AsTime().After(now) {
			unknown = true
			continue
		}
		if now.Sub(at.AsTime()) <= fleetWorkerPollerMaxAge {
			return "recent_poller"
		}
	}
	if unknown {
		return "unknown"
	}
	return "no_recent_poller"
}

func (o *fleetWorkerHealthObserver) observe(gaggle string, observation *fleetstate.Observation, attrs map[string]any) {
	if o == nil {
		return
	}
	scope, known := o.gaggles[gaggle]
	state := "unknown"
	if known && !scope.unknown {
		state = "not_required"
		if scope.engine {
			state = o.state
		}
	}
	attrs["workerObservation"] = state
	attrs["workerObservedAt"] = o.observedAt.UTC().Format(time.RFC3339Nano)
	attrs["workerCoverage"] = "engine_workflow_activity_queue"
	switch state {
	case "unknown":
		observation.Complete = false
	case "recent_poller", "no_recent_poller":
		count := 0
		if state == "no_recent_poller" {
			count = 1
		}
		observation.MissingWorkerCount = &count
		attrs["missingWorkerCount"] = count
	}
}
