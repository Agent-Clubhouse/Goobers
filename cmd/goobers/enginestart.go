package main

import (
	"context"
	"flag"
	"io"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
	"go.temporal.io/sdk/client"
)

// runEngineStart dispatches ONE run onto the tier-3 engine path.
//
// The consumer half of engine dispatch already shipped — `goobers worker` polls
// a task queue. The producer half did not: nothing in the CLI ever called
// engine.NewTemporalStarter, so a run could only ever be started on the local
// runner. That made the engine path unreachable outside tests, and it is why
// this command exists.
//
// It is deliberately narrow. It pins the workflow definition into the run at
// start (the registry snapshot, so a later re-registration cannot change a run
// in flight) and hands it to Temporal. Everything after that is the engine's.
//
// ON THE ONE TASK QUEUE, since the obvious reading is wrong: NewTemporalStarter
// takes exactly one queue, and that is correct — it is the WORKFLOW's queue, and
// a workflow runs in one place. Individual stages are not bound by it. Temporal
// carries a task queue per ACTIVITY (ActivityOptions.TaskQueue), so a stage that
// declares os=windows is polled by a Windows worker while the workflow itself
// stays put. Per-stage placement lives in engine.stageActivityOptions, not here.
// See internal/engine/placement_test.go.
const engineStartHelp = "Usage: goobers engine-start [flags] <workflow> [path]\n\n" +
	"Dispatch one run onto the tier-3 engine (experimental): pin the workflow\n" +
	"definition, connect to Temporal, and start the engine workflow on a task\n" +
	"queue a `goobers worker` is serving. The run id is derived from\n" +
	"gaggle+workflow+--dedupe-key, so starting the same unit of work twice is\n" +
	"rejected as already running rather than duplicated.\n\n" +
	"This queue is the workflow's. A stage that declares a platform dispatches to\n" +
	"\"<queue>-<goos>\" instead, so one run can span operating systems as long as a\n" +
	"worker is serving each derived queue.\n\n" +
	"Exit codes: 0 = started, 1 = dispatch failure, 2 = usage/config error.\n"

func runEngineStart(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("engine-start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gaggle := fs.String("gaggle", "", "gaggle owning the workflow")
	hostPort := fs.String("temporal-hostport", workerEnvOr("GOOBERS_TEMPORAL_HOSTPORT", "127.0.0.1:7233"), "Temporal frontend host:port")
	namespace := fs.String("temporal-namespace", workerEnvOr("GOOBERS_TEMPORAL_NAMESPACE", "default"), "Temporal namespace")
	taskQueue := fs.String("task-queue", workerEnvOr("GOOBERS_TASK_QUEUE", "goobers-spike"), "task queue to dispatch onto")
	dedupe := fs.String("dedupe-key", "", "dedupe key; the run id is derived from gaggle+workflow+key, so a repeat is rejected as already running")
	fs.Usage = helpUsage(stderr, "engine-start")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		pf(stderr, "usage: goobers engine-start [flags] <workflow> [path]\n")
		return 2
	}
	workflowName := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 2
	}
	set, _, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		pf(stderr, "error: load config directory: %v\n", err)
		return 2
	}
	instance.ApplyGaggleCICommand(set)

	target := *gaggle
	if target == "" {
		for i := range set.Workflows {
			if set.Workflows[i].Name == workflowName {
				target = set.Workflows[i].Spec.Gaggle
				break
			}
		}
	}
	if target == "" {
		pf(stderr, "error: workflow %q not found; pass --gaggle to disambiguate\n", workflowName)
		return 2
	}

	reg := engine.NewRegistryWithPreviewFeatures(set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations))
	registered := 0
	for i := range set.Workflows {
		w := set.Workflows[i]
		if w.Spec.Gaggle != target {
			continue
		}
		if _, err := reg.Register(w.Name, w.Spec); err != nil {
			pf(stderr, "error: register workflow %q: %v\n", w.Name, err)
			return 1
		}
		registered++
	}
	if registered == 0 {
		pf(stderr, "error: no workflows registered for gaggle %q\n", target)
		return 2
	}

	in, err := reg.StartInput(workflowName, engine.StartSpec{
		RunID:           engine.RunID(target, workflowName, *dedupe),
		Gaggle:          target,
		RepoRef:         gaggleProjectRef(set, target),
		TriggerKind:     "manual",
		BranchNamespace: branchNamespacesByGaggle(set)[target],
	})
	if err != nil {
		pf(stderr, "error: pin workflow %q: %v\n", workflowName, err)
		return 1
	}
	_ = cfg

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: *hostPort, Namespace: *namespace})
	if err != nil {
		pf(stderr, "error: dial temporal at %s: %v\n", *hostPort, err)
		return 1
	}
	defer c.Close()

	res, err := engine.NewTemporalStarter(c, *taskQueue).Start(ctx, in)
	if err != nil {
		pf(stderr, "error: start engine run: %v\n", err)
		return 1
	}
	pf(stdout, "engine run started: %s (workflow=%s v%d, gaggle=%s, queue=%s)\n",
		res.RunID, in.WorkflowName, in.Version, in.Gaggle, *taskQueue)
	pf(stdout, "  digest: %s\n", in.WorkflowDigest)
	return 0
}

// gaggleProjectRef is the gaggle's project repo, zero when not configured —
// which leaves credentials on the first-repo default, matching the daemon's
// legacy-runtime path.
func gaggleProjectRef(set *instance.ConfigSet, gaggle string) apiv1.RepoRef {
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			return set.Gaggles[i].Spec.Project
		}
	}
	return apiv1.RepoRef{}
}
