package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
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
	def, ok := reg.Latest(workflowName)
	if !ok {
		pf(stderr, "error: workflow %q is not registered\n", workflowName)
		return 1
	}
	spec, err := engineStartSpec(cfg, set, target, workflowName, *dedupe, project, def, *liveJournal)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	in, err := reg.StartInput(workflowName, spec)
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

// engineStartSpec builds the StartSpec one `goobers engine-start` dispatch
// pins. It is the engine-side counterpart of the daemon's scheduler entry
// (schedulerDefinitions): everything a run's identity commits to at start and
// never re-reads from config is resolved here, so both starters can be
// compared at one seam instead of two divergent literals.
func engineStartSpec(
	cfg *instance.Config,
	set *instance.ConfigSet,
	gaggle, workflowName, dedupeKey string,
	project apiv1.RepoRef,
	def workflow.Definition,
	liveJournal bool,
) (engine.StartSpec, error) {
	// Mode-3 placement pinning (#3588): resolve every stage's execution
	// placement now, against the declared runner inventory, and pin the
	// outcome into the run input — the workflow reads it as data and never
	// solves mid-run (the WF-016 snapshot / determinism constraint). Nil on
	// every zero-declaration and local-mode instance.
	placements, err := bootstrap.PinStagePlacements(cfg, set, gaggle, def)
	if err != nil {
		return engine.StartSpec{}, fmt.Errorf("resolve stage placements: %w", err)
	}
	// #3820: run controls are pinned at start and the watchdog enforces the
	// pinned value, so a starter that skips this resolution silently commits
	// the run to the 3-repass / 45m defaults whatever the author declared.
	controls, err := engineStartRunControls(cfg, set, gaggle, workflowName, project)
	if err != nil {
		return engine.StartSpec{}, err
	}
	return engine.StartSpec{
		RunID:           engine.RunID(gaggle, workflowName, dedupeKey),
		Gaggle:          gaggle,
		RepoRef:         project,
		TriggerKind:     "manual",
		BranchNamespace: branchNamespacesByGaggle(set)[gaggle],
		LiveJournal:     liveJournal,
		Placements:      placements,
		RunControls:     controls,
	}, nil
}

// engineStartRunControls resolves the run-control policy for one manually
// dispatched workflow through resolveWorkflowRunControls — the same four
// layers, in the same order, the daemon's scheduler entry resolves.
func engineStartRunControls(cfg *instance.Config, set *instance.ConfigSet, gaggle, workflowName string, project apiv1.RepoRef) (apiv1.RunControls, error) {
	var gaggleCfg apiv1.Gaggle
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			gaggleCfg = set.Gaggles[i]
			break
		}
	}
	var workflowCfg apiv1.Workflow
	declared := false
	for i := range set.Workflows {
		if set.Workflows[i].Name == workflowName && set.Workflows[i].Spec.Gaggle == gaggle {
			workflowCfg = set.Workflows[i]
			declared = true
			break
		}
	}
	if !declared {
		return apiv1.RunControls{}, fmt.Errorf("workflow %q is not declared in gaggle %q", workflowName, gaggle)
	}
	controls, err := resolveWorkflowRunControls(cfg, project, gaggleCfg, workflowCfg)
	if err != nil {
		return apiv1.RunControls{}, fmt.Errorf("workflow %q run controls: %w", workflowName, err)
	}
	return controls.Overrides(), nil
}
