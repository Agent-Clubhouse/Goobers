package workerrouting

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/temporaldial"
)

// State is the fleet-level routing verdict.
type State string

// Fleet routing states. Unknown is never reported as healthy: it means the
// check could not see the routing or the workers it would be checked against.
const (
	// StateHealthy: every live worker build is routed workflow tasks.
	StateHealthy State = "healthy"
	// StateStalled: no live worker is routed workflow tasks, so the engine
	// accepts no new work until an operator changes the routing.
	StateStalled State = "stalled"
	// StateDegraded: some live worker builds are routed nothing while
	// others still serve, as after a versioned rollout nobody promoted.
	StateDegraded State = "degraded"
	// StateUnknown: the routing or the live workers could not be read.
	StateUnknown State = "unknown"
)

// Per-worker routing values in an Assessment.
const (
	WorkerRouted   = "routed"
	WorkerUnrouted = "unrouted"
	WorkerUnknown  = "unknown"
)

const (
	// PollerMaxAge is how recently a poller must have polled to count as a
	// live worker. A versioned long poll returns within about a minute, and
	// the server keeps a departed worker listed for minutes, so a stricter
	// window keeps a replaced build from masking its successor's stall.
	PollerMaxAge         = 2 * time.Minute
	pollerClockTolerance = 5 * time.Second
)

// Client is the slice of Temporal the fleet assessment reads. NewClient
// adapts an SDK client; tests substitute a fake deployment.
type Client interface {
	// DescribeRouting returns the "goobers" worker deployment's routing.
	DescribeRouting(ctx context.Context) (client.WorkerDeploymentRoutingConfig, error)
	// WorkflowPollers returns the workflow-task pollers of taskQueue.
	WorkflowPollers(ctx context.Context, taskQueue string) ([]*taskqueuepb.PollerInfo, error)
}

// NewClient adapts an SDK client to Client.
func NewClient(c client.Client) Client { return sdkClient{c: c} }

type sdkClient struct{ c client.Client }

func (s sdkClient) DescribeRouting(ctx context.Context) (client.WorkerDeploymentRoutingConfig, error) {
	return DescribeRouting(ctx, s.c)
}

// WorkflowPollers uses the legacy DescribeTaskQueue, which lists versioned
// pollers with their deployment options beside unversioned ones (verified
// against a dev server; see the integration test). The server may answer
// from the root partition alone, so on a multi-partition queue a live worker
// whose pollers all sit on other partitions can be missed. That errs toward
// alarm (a version reported without a live worker), never toward a false
// healthy; the enhanced per-version poller reports belong to the deprecated
// build-ID versioning API and do not describe worker deployments.
func (s sdkClient) WorkflowPollers(ctx context.Context, taskQueue string) ([]*taskqueuepb.PollerInfo, error) {
	resp, err := s.c.DescribeTaskQueue(ctx, taskQueue, enumspb.TASK_QUEUE_TYPE_WORKFLOW)
	if err != nil {
		return nil, err
	}
	return resp.GetPollers(), nil
}

// Assessment is the fleet-level routing report `goobers status` and
// `goobers doctor` render. Its JSON shape is part of `status --json`.
type Assessment struct {
	State      State  `json:"state"`
	Deployment string `json:"deployment"`
	TaskQueue  string `json:"taskQueue"`
	// CurrentVersion and RampingVersion are the deployment's routing targets
	// (<deployment>.<build>, or __unversioned__); empty when unread.
	CurrentVersion    string  `json:"currentVersion,omitempty"`
	RampingVersion    string  `json:"rampingVersion,omitempty"`
	RampingPercentage float32 `json:"rampingPercentage,omitempty"`
	// Workers groups the task queue's live workflow pollers by the version
	// they serve.
	Workers []WorkerRouting `json:"workers,omitempty"`
	Detail  string          `json:"detail"`
	// Remediation lists Temporal CLI commands (set-current-version, or
	// set-ramping-version --delete) that move new workflow tasks onto live
	// workers; each line is one alternative. Goobers never runs them (#4084).
	Remediation []string  `json:"remediation,omitempty"`
	CheckedAt   time.Time `json:"checkedAt"`
}

// WorkerRouting is one served version among the live pollers, with the
// percentage of new workflow tasks the deployment routes to it.
type WorkerRouting struct {
	Version        string  `json:"version"`
	Pollers        int     `json:"pollers"`
	Routing        string  `json:"routing"`
	TrafficPercent float32 `json:"trafficPercent"`
}

// Target names the Temporal frontend and engine task queue to assess.
type Target struct {
	HostPort  string
	Namespace string
	TaskQueue string
	TLS       *temporaldial.TLS
}

// Probe dials target and assesses it. A frontend it cannot reach is
// StateUnknown with the reason; ctx bounds the dial and both reads.
func Probe(ctx context.Context, target Target, now time.Time) Assessment {
	c, err := temporaldial.Dial(ctx, target.HostPort, target.Namespace, target.TLS)
	if err != nil {
		return Unknown(target.TaskQueue, fmt.Sprintf("cannot reach Temporal frontend %s over %s: %v",
			target.HostPort, target.TLS.Transport(), err), now)
	}
	defer c.Close()
	return Assess(ctx, NewClient(c), target.TaskQueue, now)
}

// Unknown is an assessment that could not run, with the reason.
func Unknown(taskQueue, reason string, now time.Time) Assessment {
	return Assessment{
		State: StateUnknown, Deployment: DeploymentName, TaskQueue: taskQueue,
		Detail: reason, CheckedAt: now.UTC(),
	}
}

// Assess reads the live workflow pollers of taskQueue and the deployment's
// routing, and reports whether every live worker build receives tasks. It
// only reads; any failure is StateUnknown with the reason.
func Assess(ctx context.Context, c Client, taskQueue string, now time.Time) Assessment {
	pollers, err := c.WorkflowPollers(ctx, taskQueue)
	if err != nil {
		return Unknown(taskQueue, fmt.Sprintf("cannot list the workers polling task queue %q: %v", taskQueue, err), now)
	}
	routing, describeErr := c.DescribeRouting(ctx)
	return evaluate(taskQueue, routing, describeErr, pollers, now)
}

// livePollerGroups groups recent pollers by served version: unversioned
// first, then builds in order. Pollers versioned under another deployment
// are not goobers workers and are left out.
func livePollerGroups(pollers []*taskqueuepb.PollerInfo, now time.Time) []pollerGroup {
	counts := map[pollerGroup]int{}
	for _, poller := range pollers {
		if !pollerLive(poller, now) {
			continue
		}
		key := pollerGroup{}
		if opts := poller.GetDeploymentOptions(); opts != nil && opts.GetWorkerVersioningMode() == enumspb.WORKER_VERSIONING_MODE_VERSIONED {
			if opts.GetDeploymentName() != DeploymentName {
				continue
			}
			key = pollerGroup{version: version{versioned: true, build: opts.GetBuildId()}}
		}
		counts[key]++
	}
	groups := make([]pollerGroup, 0, len(counts))
	for key, count := range counts {
		key.count = count
		groups = append(groups, key)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].versioned != groups[j].versioned {
			return !groups[i].versioned
		}
		return groups[i].build < groups[j].build
	})
	return groups
}

// pollerLive keeps pollers seen within PollerMaxAge. A poller with no usable
// timestamp is kept: it is listed, and dropping it could hide a mismatch.
func pollerLive(poller *taskqueuepb.PollerInfo, now time.Time) bool {
	at := poller.GetLastAccessTime()
	if at == nil || at.CheckValid() != nil || at.AsTime().IsZero() {
		return true
	}
	return now.Sub(at.AsTime()) <= PollerMaxAge || at.AsTime().After(now.Add(pollerClockTolerance))
}

// deploymentAbsent reports a describe error meaning no deployment routing
// exists: no versioned worker ever registered it, or the server has no
// worker-deployment API. Either way new tasks go to unversioned workers.
func deploymentAbsent(err error) bool {
	var notFound *serviceerror.NotFound
	var unimplemented *serviceerror.Unimplemented
	return errors.As(err, &notFound) || errors.As(err, &unimplemented)
}

// StatusLine renders the assessment for `goobers status` text output: a
// warning with the remediation for a mismatch, the reason when it could not
// be checked, and nothing when healthy.
func (a Assessment) StatusLine() string {
	switch a.State {
	case StateHealthy:
		return ""
	case StateUnknown:
		return fmt.Sprintf("Engine routing: unknown (%s)\n", a.Detail)
	}
	var line strings.Builder
	fmt.Fprintf(&line, "Warning: engine routing %s: %s.", a.State, a.Detail)
	if len(a.Remediation) > 0 {
		fmt.Fprintf(&line, " Run: %s", strings.Join(a.Remediation, " (or) "))
	}
	line.WriteString("\n")
	return line.String()
}
