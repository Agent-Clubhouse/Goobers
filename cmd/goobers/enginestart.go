package main

import (
	"context"
	"flag"
	"io"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
)

const engineStartHelp = "Usage: goobers engine-start [flags] <workflow> [path]\n\n" +
	"Dispatch one run onto the tier-3 engine (experimental). The run id is\n" +
	"derived from gaggle, workflow, and --dedupe-key.\n\n" +
	"--live-journal pins live journal authorship into the run: workers emit\n" +
	"journal events through the daemon's journal plane as they happen, so the\n" +
	"run is visible mid-flight; without it the journal is projected from\n" +
	"history at close, as before. Requires the daemon's write API to be\n" +
	"reachable from every worker serving the run (worker --daemon-api).\n\n" +
	"Exit codes: 0 = started, 1 = dispatch failure, 2 = usage/config error.\n"

func runEngineStart(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("engine-start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gaggle := fs.String("gaggle", "", "gaggle owning the workflow")
	hostPort := fs.String("temporal-hostport", "", "Temporal frontend host:port")
	namespace := fs.String("temporal-namespace", "", "Temporal namespace")
	taskQueue := fs.String("task-queue", "", "task queue to dispatch onto")
	dedupe := fs.String("dedupe-key", "", "dedupe key used to derive the run id")
	liveJournal := fs.Bool("live-journal", false, "author the run journal live through the daemon's journal plane (DS4)")
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
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
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

	reg, project, err := bootstrap.RegisterGaggleWorkflows(set, target)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	in, err := reg.StartInput(workflowName, engine.StartSpec{
		RunID:           engine.RunID(target, workflowName, *dedupe),
		Gaggle:          target,
		RepoRef:         project,
		TriggerKind:     "manual",
		BranchNamespace: branchNamespacesByGaggle(set)[target],
		LiveJournal:     *liveJournal,
	})
	if err != nil {
		pf(stderr, "error: pin workflow %q: %v\n", workflowName, err)
		return 1
	}

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
	pf(stdout, "engine run started: %s (workflow=%s v%d, gaggle=%s, queue=%s)\n", res.RunID, in.WorkflowName, in.Version, in.Gaggle, *taskQueue)
	return 0
}
