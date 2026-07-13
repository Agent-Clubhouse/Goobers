package main

import (
	"flag"
	"io"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

func runRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers run <workflow> [path]\n\n"+
			"Trigger a run of a config/ workflow manually — still honors run conditions\n"+
			"(default path \".\"). Exit codes: 0 = run created, 1 = business error\n"+
			"(unknown workflow, invalid config), 2 = usage/IO error.\n")
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

	// WorkflowVersion is registry-assigned (per-name monotonic, WF-016); no
	// registry is wired at the instance level yet, so this pins version 1
	// until #21's scheduler (or a follow-up) introduces one.
	const workflowVersion = 1
	machine, err := workflow.Compile(workflow.Definition{Name: wf.Name, Version: workflowVersion, Spec: wf.Spec})
	if err != nil {
		pf(stderr, "error: workflow %q failed to compile: %v\n", name, err)
		return 1
	}

	runID, err := telemetry.NewRunID()
	if err != nil {
		pf(stderr, "error: generate run id: %v\n", err)
		return 2
	}

	run, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        wf.Name,
		WorkflowVersion: workflowVersion,
		WorkflowDigest:  machine.Digest(),
		Gaggle:          wf.Spec.Gaggle,
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		pf(stderr, "error: create run journal: %v\n", err)
		return 2
	}
	defer func() { _ = run.Close() }()

	// TODO(#17): the local runner core isn't merged yet, so this command can
	// pin a run's identity and open its journal but cannot advance the
	// compiled machine. Recording an honest escalation (rather than silently
	// pretending the run executed) keeps `status`/`trace` truthful; once #17
	// lands, this becomes a real runner.Advance(ctx, machine, run, ...) call.
	if err := run.Append(journal.Event{
		Type: journal.EventError,
		Error: &journal.ErrorDetail{
			Code:    "runner_unavailable",
			Message: "local runner (#17) is not yet wired into the CLI; run created but no stages executed",
		},
	}); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	pf(stdout, "created run %s (workflow=%s gaggle=%s)\n", runID, wf.Name, wf.Spec.Gaggle)
	pln(stdout, "escalated: local runner (#17) not yet wired — no stages executed")
	pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
	return 0
}
