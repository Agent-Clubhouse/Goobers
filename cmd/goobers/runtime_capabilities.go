package main

import (
	"io"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/version"
)

type cliCommandHandler func([]string, io.Writer, io.Writer) int

type cliCommand struct {
	names            []string
	action           apicontract.SurfaceAction
	actionRegistered bool
	subcommands      []cliCommand
	run              cliCommandHandler
}

var cliCommands = []cliCommand{
	command("init", apicontract.ActionConfigTime, runInit),
	groupCommand(
		"scaffold",
		runScaffold,
		subcommand(
			"scaffold goober",
			"goober",
			apicontract.ActionConfigTime,
			func(args []string, stdout, stderr io.Writer) int {
				return runScaffoldKind("goober", args, stdout, stderr)
			},
		),
		subcommand(
			"scaffold workflow",
			"workflow",
			apicontract.ActionConfigTime,
			func(args []string, stdout, stderr io.Writer) int {
				return runScaffoldKind("workflow", args, stdout, stderr)
			},
		),
	),
	command("validate", apicontract.ActionConfigTime, runValidate),
	command("up", apicontract.ActionDaemonLifecycle, runUp),
	command("dashboard", apicontract.ActionReadOnlyNavigation, runDashboard),
	commandWithSubcommands(
		"run",
		apicontract.ActionWorkflowExecution,
		runRun,
		subcommand("run abort", "abort", apicontract.ActionMaintenance, runRunAbort),
	),
	command(detachedRunWorkerCommand, apicontract.ActionWorkflowExecution, runDetachedWorker),
	command("signal", apicontract.ActionWorkflowExecution, runSignal),
	groupCommand(
		"workflow",
		runWorkflow,
		subcommand("workflow show", "show", apicontract.ActionReadOnlyNavigation, runWorkflowShow),
	),
	groupCommand(
		"runs",
		runRuns,
		subcommand("runs list", "list", apicontract.ActionReadOnlyNavigation, runRunsList),
		subcommand("runs du", "du", apicontract.ActionReadOnlyNavigation, runRunsDU),
	),
	command("status", apicontract.ActionReadOnlyNavigation, runStatus),
	command("stats", apicontract.ActionReadOnlyNavigation, runStats),
	command("trace", apicontract.ActionReadOnlyNavigation, runTrace),
	commandWithSubcommands(
		"escalations",
		apicontract.ActionReadOnlyNavigation,
		runEscalations,
		subcommand("escalations show", "show", apicontract.ActionReadOnlyNavigation, runEscalationShow),
	),
	groupCommand(
		"completion",
		runCompletion,
		subcommand(
			"completion bash",
			"bash",
			apicontract.ActionConfigTime,
			func(args []string, stdout, stderr io.Writer) int {
				return runCompletionScript(bashCompletion, args, stdout, stderr)
			},
		),
		subcommand(
			"completion zsh",
			"zsh",
			apicontract.ActionConfigTime,
			func(args []string, stdout, stderr io.Writer) int {
				return runCompletionScript(zshCompletion, args, stdout, stderr)
			},
		),
		subcommand(
			"completion fish",
			"fish",
			apicontract.ActionConfigTime,
			func(args []string, stdout, stderr io.Writer) int {
				return runCompletionScript(fishCompletion, args, stdout, stderr)
			},
		),
	),
	command("__complete", apicontract.ActionConfigTime, func(args []string, stdout, _ io.Writer) int {
		return runCompletionCandidates(args, stdout)
	}),
	groupCommand(
		"telemetry",
		runTelemetry,
		subcommand("telemetry stats", "stats", apicontract.ActionReadOnlyNavigation, runTelemetryStats),
		subcommand("telemetry errors", "errors", apicontract.ActionReadOnlyNavigation, runTelemetryErrors),
	),
	command("telemetry-query", apicontract.ActionWorkflowExecution, runTelemetryQuery),
	command("docs-churn", apicontract.ActionWorkflowExecution, runDocsChurn),
	groupCommand(
		"journal",
		runJournal,
		subcommand("journal redact", "redact", apicontract.ActionMaintenance, runJournalRedact),
	),
	groupCommand(
		"blocked",
		runBlocked,
		subcommand("blocked list", "list", apicontract.ActionReadOnlyNavigation, runBlockedList),
		subcommand("blocked clear", "clear", apicontract.ActionMaintenance, runBlockedClear),
	),
	groupCommand(
		"claims",
		runClaims,
		subcommand("claims list", "list", apicontract.ActionReadOnlyNavigation, runClaimsList),
		subcommand("claims release", "release", apicontract.ActionMaintenance, runClaimsRelease),
	),
	command("backlog-query", apicontract.ActionWorkflowExecution, runBacklogQuery),
	command("reconcile-branches", apicontract.ActionWorkflowExecution, runReconcileBranches),
	command("push-branch", apicontract.ActionWorkflowExecution, runPushBranch),
	command("open-pr", apicontract.ActionWorkflowExecution, runOpenPR),
	command("issue-close-out", apicontract.ActionWorkflowExecution, runIssueCloseOut),
	command("reset-rate-limit", apicontract.ActionMaintenance, runResetRateLimit),
	command("merge-pr", apicontract.ActionWorkflowExecution, runMergePR),
	command("merge-queue-poll", apicontract.ActionWorkflowExecution, runMergeQueuePoll),
	command("pr-select", apicontract.ActionWorkflowExecution, runPRSelect),
	command("gather-sibling-context", apicontract.ActionWorkflowExecution, runGatherSiblingContext),
	command("apply-verdict", apicontract.ActionWorkflowExecution, runApplyVerdict),
	command("elect-lander", apicontract.ActionWorkflowExecution, runElectLander),
	command("post-merge", apicontract.ActionWorkflowExecution, runPostMerge),
	command("update-behind-pr", apicontract.ActionWorkflowExecution, runUpdateBehindPR),
	command("gather-pr-context", apicontract.ActionWorkflowExecution, runGatherPRContext),
	command("rebase-pr", apicontract.ActionWorkflowExecution, runRebasePR),
	command("remediation-checkpoint", apicontract.ActionWorkflowExecution, runRemediationCheckpoint),
	command("push-remediated", apicontract.ActionWorkflowExecution, runPushRemediated),
	aliasCommand(
		"version",
		[]string{"--version", "version"},
		apicontract.ActionReadOnlyNavigation,
		func(_ []string, stdout, _ io.Writer) int {
			pf(stdout, "goobers %s\n", version.Get())
			return 0
		},
	),
	aliasCommand(
		"help",
		[]string{"-h", "--help", "help"},
		apicontract.ActionReadOnlyNavigation,
		func(_ []string, stdout, _ io.Writer) int {
			usage(stdout)
			return 0
		},
	),
}

func command(
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	return aliasCommand(name, []string{name}, class, handler)
}

func commandWithSubcommands(
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
	subcommands ...cliCommand,
) cliCommand {
	registration := command(name, class, handler)
	registration.subcommands = subcommands
	return registration
}

func groupCommand(
	name string,
	handler cliCommandHandler,
	subcommands ...cliCommand,
) cliCommand {
	return cliCommand{
		names:       []string{name},
		subcommands: subcommands,
		run:         handler,
	}
}

func subcommand(
	id string,
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	return aliasCommand(id, []string{name}, class, handler)
}

func runtimeCommand(
	name string,
	capability apicontract.CapabilityID,
	handler cliCommandHandler,
) cliCommand {
	return withRuntimeCapability(
		command(name, apicontract.ActionRuntimeMutation, handler),
		capability,
	)
}

func runtimeSubcommand(
	id string,
	name string,
	capability apicontract.CapabilityID,
	handler cliCommandHandler,
) cliCommand {
	return withRuntimeCapability(
		subcommand(id, name, apicontract.ActionRuntimeMutation, handler),
		capability,
	)
}

func withRuntimeCapability(
	registration cliCommand,
	capability apicontract.CapabilityID,
) cliCommand {
	registration.action.Capability = capability
	return registration
}

func aliasCommand(
	id string,
	names []string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	return cliCommand{
		names: names,
		action: apicontract.SurfaceAction{
			ID:    apicontract.ActionID(id),
			Class: class,
		},
		actionRegistered: true,
		run:              handler,
	}
}

func findCLICommand(name string) (cliCommand, bool) {
	return findCLICommandIn(cliCommands, name)
}

func findCLICommandIn(commands []cliCommand, name string) (cliCommand, bool) {
	for _, command := range commands {
		for _, candidate := range command.names {
			if candidate == name {
				return command, true
			}
		}
	}
	return cliCommand{}, false
}

func (command cliCommand) dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if subcommand, ok := findCLICommandIn(command.subcommands, args[0]); ok {
			return subcommand.dispatch(args[1:], stdout, stderr)
		}
	}
	return command.run(args, stdout, stderr)
}

func cliSurfaceActions() []apicontract.SurfaceAction {
	return cliSurfaceActionsFrom(cliCommands)
}

func cliSurfaceActionsFrom(commands []cliCommand) []apicontract.SurfaceAction {
	var actions []apicontract.SurfaceAction
	for _, command := range commands {
		if command.actionRegistered {
			actions = append(actions, command.action)
		}
		actions = append(actions, cliSurfaceActionsFrom(command.subcommands)...)
	}
	return actions
}
