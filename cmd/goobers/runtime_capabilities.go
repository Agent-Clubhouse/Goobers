package main

import (
	"io"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/version"
)

type cliCommandHandler func([]string, io.Writer, io.Writer) int

type cliCommand struct {
	names  []string
	action apicontract.SurfaceAction
	run    cliCommandHandler
}

var cliCommands = []cliCommand{
	command("init", apicontract.ActionConfigTime, runInit),
	command("scaffold", apicontract.ActionConfigTime, runScaffold),
	command("validate", apicontract.ActionConfigTime, runValidate),
	command("up", apicontract.ActionDaemonLifecycle, runUp),
	command("run", apicontract.ActionWorkflowExecution, runRun),
	command(detachedRunWorkerCommand, apicontract.ActionWorkflowExecution, runDetachedWorker),
	command("signal", apicontract.ActionWorkflowExecution, runSignal),
	command("workflow", apicontract.ActionReadOnlyNavigation, runWorkflow),
	command("runs", apicontract.ActionReadOnlyNavigation, runRuns),
	command("status", apicontract.ActionReadOnlyNavigation, runStatus),
	command("stats", apicontract.ActionReadOnlyNavigation, runStats),
	command("trace", apicontract.ActionReadOnlyNavigation, runTrace),
	command("completion", apicontract.ActionConfigTime, runCompletion),
	command("__complete", apicontract.ActionConfigTime, func(args []string, stdout, _ io.Writer) int {
		return runCompletionCandidates(args, stdout)
	}),
	command("telemetry", apicontract.ActionReadOnlyNavigation, runTelemetry),
	command("telemetry-query", apicontract.ActionWorkflowExecution, runTelemetryQuery),
	command("journal", apicontract.ActionMaintenance, runJournal),
	command("backlog-query", apicontract.ActionWorkflowExecution, runBacklogQuery),
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

func runtimeCommand(
	name string,
	capability apicontract.CapabilityID,
	handler cliCommandHandler,
) cliCommand {
	registration := command(name, apicontract.ActionRuntimeMutation, handler)
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
		run: handler,
	}
}

func findCLICommand(name string) (cliCommand, bool) {
	for _, command := range cliCommands {
		for _, candidate := range command.names {
			if candidate == name {
				return command, true
			}
		}
	}
	return cliCommand{}, false
}

func cliSurfaceActions() []apicontract.SurfaceAction {
	actions := make([]apicontract.SurfaceAction, 0, len(cliCommands))
	for _, command := range cliCommands {
		actions = append(actions, command.action)
	}
	return actions
}
