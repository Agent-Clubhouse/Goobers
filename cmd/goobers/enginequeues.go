package main

// enginequeues.go is the far-side evidence surface for decision 003's queue
// ownership claim (record item 11): "DescribeTaskQueue shows goobers-worker
// pollers on the workflow queue and every goobers-dispatch.* queue, none from
// goobers-api."
//
// That claim is about WHICH PROCESS polls WHICH queue, and it is exactly the
// misconfiguration the record's rejected option B would re-create silently
// (R7): the daemon is a Temporal CLIENT — it starts DispatchOne workflows and
// waits on them — and must never become a poller, because a poller on a
// dispatch queue is a process that would execute DispatchStage, i.e. create
// pods. Nothing in the daemon's own logs distinguishes "client" from "poller";
// only the frontend knows. This command asks it, so the cluster check can
// assert the answer instead of assuming it.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
)

const engineQueuesHelp = "Usage: goobers engine-queues [flags] [path]\n\n" +
	"Report which workers poll this instance's engine task queues (experimental).\n" +
	"The queue set is derived, not typed: the engine's workflow queue plus every\n" +
	"goobers-dispatch.<gaggle>.<runner> queue the runners: inventory and the\n" +
	"declared gaggles imply — the same derivation `goobers worker\n" +
	"--dispatch-namespace` serves. Each queue is described for both task types\n" +
	"(workflow and activity) and every poller's Temporal identity is printed.\n\n" +
	"This is the evidence for the queue-ownership check: goobers-worker must poll\n" +
	"the workflow queue and every dispatch queue, and goobers-api must poll none\n" +
	"of them — the daemon dispatches as a Temporal client and is never a pod\n" +
	"creator.\n\n" +
	"Flags:\n" +
	"  --temporal-hostport <h:p>  Temporal frontend (default engine.hostPort)\n" +
	"  --temporal-namespace <ns>  Temporal namespace (default engine.namespace)\n" +
	"  --task-queue <queue>       workflow queue to describe (default\n" +
	"                             engine.taskQueue)\n" +
	"  --timeout <duration>       bound on the whole describe (default 30s)\n" +
	"  --json                     emit the report as JSON\n\n" +
	"Exit codes: 0 = described, 1 = describe/connection failure, 2 = usage/config\n" +
	"error.\n"

// queuePollers is one described (queue, task type) pair.
type queuePollers struct {
	Queue string `json:"queue"`
	// Type is "workflow" or "activity".
	Type string `json:"type"`
	// Dispatch marks a goobers-dispatch.* queue, so a checker can select the
	// dispatch set without re-deriving the prefix.
	Dispatch bool `json:"dispatch"`
	// Pollers are the Temporal worker identities polling this pair, sorted.
	Pollers []string `json:"pollers"`
}

func runEngineQueues(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("engine-queues", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hostPort := fs.String("temporal-hostport", "", "Temporal frontend host:port")
	namespace := fs.String("temporal-namespace", "", "Temporal namespace")
	taskQueue := fs.String("task-queue", "", "workflow task queue to describe")
	timeout := fs.Duration("timeout", 30*time.Second, "bound on the whole describe")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	fs.Usage = helpUsage(stderr, "engine-queues")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		pf(stderr, "usage: goobers engine-queues [flags] [path]\n")
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 2
	}
	engineConfig := cfg.EffectiveEngineConfig()
	if *hostPort == "" {
		*hostPort = engineConfig.HostPort
	}
	if *namespace == "" {
		*namespace = engineConfig.Namespace
	}
	if *taskQueue == "" {
		*taskQueue = engineConfig.TaskQueue
	}
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		printValidationIssues(stderr, report)
		pf(stderr, "error: load config directory: %v\n", err)
		return 2
	}
	dispatchQueues, err := instanceDispatchQueues(cfg, set)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := dialEngineTemporal(ctx, client.Options{HostPort: *hostPort, Namespace: *namespace})
	if err != nil {
		pf(stderr, "error: dial temporal at %s: %v\n", *hostPort, err)
		return 1
	}
	defer c.Close()

	rows, err := describeEngineQueues(ctx, c, *taskQueue, dispatchQueues)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if *asJSON {
		encoded, merr := json.MarshalIndent(rows, "", "  ")
		if merr != nil {
			pf(stderr, "error: encode queue report: %v\n", merr)
			return 1
		}
		pln(stdout, string(encoded))
		return 0
	}
	printEngineQueues(stdout, rows)
	if len(dispatchQueues) == 0 {
		pf(stdout, "note: this instance declares no non-self runner, so it implies no %s.* queue\n", dispatcher.QueuePrefix)
	}
	return 0
}

// instanceDispatchQueues derives the goobers-dispatch.* queue set exactly the
// way buildStageDispatch does — through dispatcher.Queues over the resolved
// runner inventory and the declared gaggles. Deriving it a second way here
// would let the check pass against a queue set the worker does not actually
// serve, which is the one failure a queue-ownership check must not have.
func instanceDispatchQueues(cfg *instance.Config, set *instance.ConfigSet) ([]string, error) {
	if cfg == nil || set == nil {
		return nil, nil
	}
	specs := make([]dispatcher.RunnerSpec, 0, len(cfg.Runners))
	for _, entry := range cfg.ResolvedRunners() {
		spec, err := dispatcher.SpecFromEntry(entry)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	gaggles := make([]string, 0, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggles = append(gaggles, set.Gaggles[i].Name)
	}
	return dispatcher.Queues(gaggles, specs), nil
}

// engineTaskQueueDescriber is the one call this command makes; client.Client
// satisfies it and tests substitute a fake.
type engineTaskQueueDescriber interface {
	DescribeTaskQueue(ctx context.Context, taskqueue string, taskqueueType enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error)
}

// describeEngineQueues describes the workflow queue and every dispatch queue
// for BOTH task types. Both types matter: a worker that polls a dispatch
// queue's activity type is the one that runs DispatchStage and creates pods,
// while the workflow type is what carries DispatchOne — a check that looked at
// only one of them could see an empty answer on a correctly served queue.
func describeEngineQueues(ctx context.Context, c engineTaskQueueDescriber, workflowQueue string, dispatchQueues []string) ([]queuePollers, error) {
	queues := make([]string, 0, 1+len(dispatchQueues))
	if strings.TrimSpace(workflowQueue) != "" {
		queues = append(queues, workflowQueue)
	}
	queues = append(queues, dispatchQueues...)

	types := []struct {
		name string
		kind enumspb.TaskQueueType
	}{
		{"workflow", enumspb.TASK_QUEUE_TYPE_WORKFLOW},
		{"activity", enumspb.TASK_QUEUE_TYPE_ACTIVITY},
	}
	rows := make([]queuePollers, 0, len(queues)*len(types))
	for _, queue := range queues {
		for _, t := range types {
			desc, err := c.DescribeTaskQueue(ctx, queue, t.kind)
			if err != nil {
				return nil, fmt.Errorf("describe task queue %s (%s): %w", queue, t.name, err)
			}
			identities := make([]string, 0, len(desc.GetPollers()))
			for _, poller := range desc.GetPollers() {
				identities = append(identities, poller.GetIdentity())
			}
			sort.Strings(identities)
			rows = append(rows, queuePollers{
				Queue:    queue,
				Type:     t.name,
				Dispatch: strings.HasPrefix(queue, dispatcher.QueuePrefix+"."),
				Pollers:  identities,
			})
		}
	}
	return rows, nil
}

// printEngineQueues renders the report as aligned text. A queue with no
// pollers prints an explicit "(none)" rather than an empty column, because
// "nobody polls this" is the finding the check most needs to read.
func printEngineQueues(w io.Writer, rows []queuePollers) {
	width := len("QUEUE")
	for _, row := range rows {
		if len(row.Queue) > width {
			width = len(row.Queue)
		}
	}
	pf(w, "%-*s  %-8s  %s\n", width, "QUEUE", "TYPE", "POLLERS")
	for _, row := range rows {
		pollers := "(none)"
		if len(row.Pollers) > 0 {
			pollers = strings.Join(row.Pollers, ", ")
		}
		pf(w, "%-*s  %-8s  %s\n", width, row.Queue, row.Type, pollers)
	}
}
