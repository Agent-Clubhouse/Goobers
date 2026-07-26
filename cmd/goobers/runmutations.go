package main

import (
	"flag"
	"io"
)

// Tier-2 human-intervention CLI commands (HITL-7/#469): approve/override/
// rerun. Each is registered as a first-class ActionRuntimeMutation command
// now — via runtimeSubcommand, so it appears in cliSurfaceActions() and
// participates in the CLI/API/UI runtime-parity check alongside the stub
// routes internal/httpapi already registers — but the handler body is a
// deliberate stub. The real behavior (calling the daemon's approve/override/
// rerun API through this same seam) is #466/#468's scope: gate force-pass/
// override (#468) has no backing implementation yet (internal/gate/
// evaluate.go still refuses apiv1.EvaluatorHuman outright), and nothing
// today wires internal/runner.RerunStage (the real, already-built
// rerun-with-addendum primitive, #465/#467) to any external caller. Landing
// every surface's registration now means #466/#468 only ever replace a
// handler body — never touch CLI registration, API routing, or auth wiring.

const runApproveHelp = "Usage: goobers run approve <run-id> <stage> [path]\n\n" +
	"Approve an escalated human/reviewer gate, unblocking the run past it.\n" +
	"Not yet implemented (HITL-4/#466) — this command is registered now so the\n" +
	"CLI surface, the daemon API route, and the access-control seam (HITL-7/\n" +
	"#469) are all in place before the real behavior lands.\n\n" +
	"Exit codes: 1 = not yet implemented, 2 = usage error.\n"

func runRunApprove(args []string, stdout, stderr io.Writer) int {
	return runStageMutationStub("run approve", runApproveHelp, args, stdout, stderr)
}

const runOverrideHelp = "Usage: goobers run override <run-id> <stage> [path]\n\n" +
	"Force-pass a nondeterministic gate with an operator-supplied rationale,\n" +
	"overriding its own verdict. Not yet implemented (HITL-6/#468) — this\n" +
	"command is registered now so the CLI surface, the daemon API route, and\n" +
	"the access-control seam (HITL-7/#469) are all in place before the real\n" +
	"behavior lands.\n\n" +
	"Exit codes: 1 = not yet implemented, 2 = usage error.\n"

func runRunOverride(args []string, stdout, stderr io.Writer) int {
	return runStageMutationStub("run override", runOverrideHelp, args, stdout, stderr)
}

const runRerunHelp = "Usage: goobers run rerun <run-id> <stage> [path]\n\n" +
	"Re-enter an escalated run at one agentic task or reviewer gate with a\n" +
	"one-off recorded instruction addendum. The underlying primitive already\n" +
	"exists (internal/runner.RerunStage, HITL-3/HITL-5, #465/#467) but nothing\n" +
	"outside the runner package calls it yet — this command is registered now\n" +
	"so the CLI surface, the daemon API route, and the access-control seam\n" +
	"(HITL-7/#469) are all in place before HITL-4 (#466) wires it through.\n\n" +
	"Exit codes: 1 = not yet implemented, 2 = usage error.\n"

func runRunRerun(args []string, stdout, stderr io.Writer) int {
	return runStageMutationStub("run rerun", runRerunHelp, args, stdout, stderr)
}

// runStageMutationStub is the shared stub body for every tier-2 CLI
// intervention command: it validates the shared <run-id> <stage> [path]
// shape usage errors would catch either way, then reports the action as not
// yet implemented — never a fake success, since no state actually changed.
func runStageMutationStub(id, help string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(id, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, id)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 || fs.NArg() > 3 {
		fs.Usage()
		return 2
	}
	pf(stderr, "error: %s is not implemented yet (tracked separately from the access-control seam, HITL-7/#469)\n", id)
	return 1
}
