package main

import (
	"context"
	"flag"
	"io"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/telemetry"
)

func runRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers run <workflow> [path]\n\n"+
			"Trigger a run of a config/ workflow manually and wait for it to reach a\n"+
			"terminal state or pause (default path \".\"). Exit codes: 0 = run created\n"+
			"and dispatched, 1 = business error (unknown workflow, invalid config, run\n"+
			"failed to dispatch), 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	set, _, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		pf(stderr, "error: config directory invalid: %v\n", err)
		return 1
	}

	var wf *apiv1.Workflow
	for i := range set.Workflows {
		if set.Workflows[i].Name == name {
			wf = &set.Workflows[i]
			break
		}
	}
	if wf == nil {
		pf(stderr, "error: no workflow named %q in %s\n", name, l.ConfigDir())
		return 1
	}

	goobers := goobersByName(set)
	machines, err := compiledMachines(set, goobers)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	repoRefs, err := repoRefsByWorkflow(set)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx, stop := signals.SetupSignalContext()
	defer stop()

	tel, err := buildTelemetryClient(ctx, l)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()

	runnerCfg, err := buildRunnerConfig(l, cfg, goobers, tel)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	rn, err := runner.New(runnerCfg)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	runID, err := telemetry.NewRunID()
	if err != nil {
		pf(stderr, "error: generate run id: %v\n", err)
		return 2
	}

	pf(stdout, "created run %s (workflow=%s gaggle=%s)\n", runID, wf.Name, wf.Spec.Gaggle)

	result, err := rn.Start(ctx, runner.StartInput{
		RunID:   runID,
		Machine: machines[wf.Name],
		Gaggle:  wf.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: repoRefs[wf.Name],
	})
	if err != nil {
		pf(stderr, "error: run %s failed: %v\n", runID, err)
		return 1
	}

	pf(stdout, "finished: phase=%s state=%s\n", result.Phase, result.FinalState)
	pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
	return 0
}
