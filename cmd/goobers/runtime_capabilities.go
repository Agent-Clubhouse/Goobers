package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/executor"
)

type cliCommandHandler func([]string, io.Writer, io.Writer) int

type cliCommand struct {
	names            []string
	action           apicontract.SurfaceAction
	actionRegistered bool
	subcommands      []cliCommand
	run              cliCommandHandler
	providerStage    bool
	resultFile       string

	// Help metadata — the single source of truth for every rendered help
	// surface (#1095, CLI-1). Both the top-level usage() and each command's own
	// `-h` output derive from these fields; nothing hand-writes help text
	// twice. short is a one-line description; long is the full `-h` help body
	// (verbatim, trailing newline included); synopsis is this command's entry in
	// the top-level usage() list (empty ⇒ omitted from top-level usage, e.g.
	// internal stages); examples are runnable invocations for the generated
	// man pages / CLI reference that CLI-2/CLI-3 build on this shape.
	short    string
	long     string
	synopsis string
	examples []string
}

// withHelp attaches the one-line short description and the full `-h` help body.
func (c cliCommand) withHelp(short, long string) cliCommand {
	c.short = short
	c.long = long
	return c
}

// withSynopsis attaches this command's verbatim entry in the top-level usage()
// list. A command with no synopsis is not shown in top-level usage.
func (c cliCommand) withSynopsis(synopsis string) cliCommand {
	c.synopsis = synopsis
	return c
}

// withExamples attaches runnable example invocations (consumed by generated
// docs/man pages, CLI-2).
func (c cliCommand) withExamples(examples ...string) cliCommand {
	c.examples = examples
	return c
}

// cliCommands is the command registry — the single source of truth for
// dispatch, runtime-capability parity, AND help (#1095, CLI-1). Command
// declaration order here is the top-level usage() display order.
//
// It is populated in init() rather than as a var initializer to break an
// initialization cycle Go's analysis would otherwise flag: the table lists
// handler func-values (e.g. runOpenPR), whose bodies now call helpUsage →
// commandHelp, which reads this very slice. Assigning in init() runs after all
// consts/vars resolve and long before any handler executes, so the read is
// always safe at runtime.
var cliCommands []cliCommand

func init() {
	cliCommands = []cliCommand{
		aliasCommand(
			"version",
			[]string{"--version", "version"},
			apicontract.ActionReadOnlyNavigation,
			runVersion,
		).
			withSynopsis(synopsisByID["version"]).
			withHelp("print build version, commit, and date (--json for structured output)", versionHelp).
			withExamples("goobers --version", "goobers version --json"),
		command("versions", apicontract.ActionReadOnlyNavigation, runVersions).
			withSynopsis(synopsisByID["versions"]).
			withHelp("print the supported DSL, Go toolchain, and OS/arch matrix (--json for structured output)", versionsHelp).
			withExamples("goobers versions", "goobers versions --json"),
		command("init", apicontract.ActionConfigTime, runInit).
			withSynopsis(synopsisByID["init"]).
			withHelp("scaffold an instance root", initHelp).
			withExamples("goobers init", "goobers init --template=quickstart ./tutorial", "goobers init --guided ./my-instance", "goobers init --demo ./demo"),
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
			).withHelp("scaffold a goober in a gaggle", scaffoldHelp),
			subcommand(
				"scaffold workflow",
				"workflow",
				apicontract.ActionConfigTime,
				func(args []string, stdout, stderr io.Writer) int {
					return runScaffoldKind("workflow", args, stdout, stderr)
				},
			).withHelp("scaffold a workflow in a gaggle", scaffoldHelp),
		).
			withSynopsis(synopsisByID["scaffold"]).
			withHelp("scaffold a goober or workflow in a gaggle", scaffoldHelp).
			withExamples("goobers scaffold goober my-coder", "goobers scaffold workflow my-flow"),
		command("validate", apicontract.ActionConfigTime, runValidate).
			withSynopsis(synopsisByID["validate"]).
			withHelp("validate an instance or checked-in config source tree", validateHelp).
			withExamples("goobers validate", "goobers validate --check-harness --check-repos"),
		command("lint", apicontract.ActionConfigTime, runLint).
			withSynopsis(synopsisByID["lint"]).
			withHelp("lint config via the single authoritative validation engine (alias for validate)", lintHelp).
			withExamples("goobers lint", "goobers lint --check-harness --check-repos"),
		groupCommand(
			"config",
			runConfig,
			subcommand("config diff", "diff", apicontract.ActionConfigTime, runConfigDiff).
				withHelp("compare active workflows with canonical definitions", configDiffHelp).
				withExamples("goobers config diff ./instance", "goobers config diff --against ./selfhost ./instance"),
			subcommand("config show", "show", apicontract.ActionReadOnlyNavigation, runConfigShow).
				withHelp("render the effective instance config (secrets redacted)", configShowHelp).
				withExamples("goobers config show", "goobers config show --json"),
		).
			withSynopsis(synopsisByID["config"]).
			withHelp("inspect instance configuration and compare workflow definitions", configHelp).
			withExamples("goobers config show", "goobers config diff ./instance"),
		command("up", apicontract.ActionDaemonLifecycle, runUp).
			withSynopsis(synopsisByID["up"]).
			withHelp("run the daemon (scheduler + runner + loopback HTTP API)", upHelp).
			withExamples("goobers up", "goobers up --quiet --notify=all"),
		groupCommand(
			"service",
			runService,
			subcommand("service install", "install", apicontract.ActionDaemonLifecycle, runServiceInstall).
				withHelp("install, enable, and start the supervised daemon", serviceInstallHelp).
				withExamples("goobers service install", "goobers service install ./instance"),
			subcommand("service uninstall", "uninstall", apicontract.ActionDaemonLifecycle, runServiceUninstall).
				withHelp("gracefully stop and remove the supervised daemon", serviceUninstallHelp).
				withExamples("goobers service uninstall", "goobers service uninstall ./instance"),
			subcommand("service status", "status", apicontract.ActionReadOnlyNavigation, runServiceStatus).
				withHelp("report whether the supervised daemon is installed and running", serviceStatusHelp).
				withExamples("goobers service status", "goobers service status --json"),
		).
			withSynopsis(synopsisByID["service"]).
			withHelp("install and manage the platform-supervised daemon", serviceHelp).
			withExamples("goobers service install", "goobers service status", "goobers service uninstall"),
		command("dashboard", apicontract.ActionReadOnlyNavigation, runDashboard).
			withSynopsis(synopsisByID["dashboard"]).
			withHelp("serve and open the local operations portal", fmt.Sprintf(dashboardHelp, defaultDashboardPort)).
			withExamples("goobers dashboard", "goobers dashboard --port=auto --no-open"),
		commandWithSubcommands(
			"run",
			apicontract.ActionWorkflowExecution,
			runRun,
			subcommand("run abort", "abort", apicontract.ActionMaintenance, runRunAbort).
				withSynopsis(synopsisByID["run abort"]).
				withHelp("mark a stuck non-terminal run aborted", runAbortHelp).
				withExamples("goobers run abort <run-id>"),
			subcommand("run cancel", "cancel", apicontract.ActionMaintenance, runRunCancel).
				withSynopsis(synopsisByID["run cancel"]).
				withHelp("cancel a live in-flight run via the daemon", runCancelHelp).
				withExamples("goobers run cancel <run-id>"),
		).
			withSynopsis(synopsisByID["run"]).
			withHelp("trigger a run manually (still honors run conditions)", runHelp).
			withExamples("goobers run default-implement", "goobers run default-implement --no-wait"),
		command(detachedRunWorkerCommand, apicontract.ActionWorkflowExecution, runDetachedWorker),
		command(demoProviderCommand, apicontract.ActionWorkflowExecution, runDemoProvider),
		command("signal", apicontract.ActionWorkflowExecution, runSignal).
			withSynopsis(synopsisByID["signal"]).
			withHelp("fire an external signal to subscribed workflows", signalHelp).
			withExamples("goobers signal deploy-approved"),
		groupCommand(
			"workflow",
			runWorkflow,
			subcommand("workflow show", "show", apicontract.ActionReadOnlyNavigation, runWorkflowShow).
				withSynopsis(synopsisByID["workflow show"]).
				withHelp("show a workflow as a text DAG", workflowShowHelp).
				withExamples("goobers workflow show default-implement", "goobers workflow show default-implement --dot"),
		).withHelp("inspect workflows", workflowHelp),
		groupCommand(
			"runs",
			runRuns,
			subcommand("runs list", "list", apicontract.ActionReadOnlyNavigation, runRunsList).
				withSynopsis(synopsisByID["runs list"]).
				withHelp("alias for the status run table (same flags, no --watch)", runsListHelp).
				withExamples("goobers runs list", "goobers runs list --json --limit=20"),
			subcommand("runs du", "du", apicontract.ActionReadOnlyNavigation, runRunsDU).
				withSynopsis(synopsisByID["runs du"]).
				withHelp("report per-run journal and artifact bytes", runsDuHelp).
				withExamples("goobers runs du", "goobers runs du --json"),
		).withHelp("list runs and report per-run disk usage", runsHelp),
		command("status", apicontract.ActionReadOnlyNavigation, runStatus).
			withSynopsis(synopsisByID["status"]).
			withHelp("validate config, show warnings, list runs, or report daemon health", statusHelp).
			withExamples("goobers status", "goobers status --daemon", "goobers status --watch"),
		command("stats", apicontract.ActionReadOnlyNavigation, runStats).
			withSynopsis(synopsisByID["stats"]).
			withHelp("show the instance lifetime summary card", statsHelp).
			withExamples("goobers stats", "goobers stats --since 24h --json"),
		command("features", apicontract.ActionReadOnlyNavigation, runFeatures).
			withSynopsis(synopsisByID["features"]).
			withHelp("list the workflow-DSL features this build supports", featuresHelp).
			withExamples("goobers features", "goobers features --dsl-version 1.4", "goobers features --used"),
		command("reset-rate-limit", apicontract.ActionMaintenance, runResetRateLimit).
			withSynopsis(synopsisByID["reset-rate-limit"]).
			withHelp("clear the hourly run-rate budget without deleting runs/", resetRateLimitHelp).
			withExamples("goobers reset-rate-limit"),
		groupCommand(
			"blocked",
			runBlocked,
			subcommand("blocked list", "list", apicontract.ActionReadOnlyNavigation, runBlockedList).
				withSynopsis(synopsisByID["blocked list"]).
				withHelp("print the learned blocked-item ledger (scheduler/blocked.json)", blockedListHelp).
				withExamples("goobers blocked list", "goobers blocked list --json"),
			subcommand("blocked clear", "clear", apicontract.ActionMaintenance, runBlockedClear).
				withSynopsis(synopsisByID["blocked clear"]).
				withHelp("safely remove one blocked-item record, under claims.lock", blockedClearHelp).
				withExamples("goobers blocked clear 955"),
		).withHelp("inspect and clear the learned blocked-item ledger", blockedHelp),
		groupCommand(
			"claims",
			runClaims,
			subcommand("claims list", "list", apicontract.ActionReadOnlyNavigation, runClaimsList).
				withSynopsis(synopsisByID["claims list"]).
				withHelp("print current claim leases, optionally only expired leases", claimsListHelp).
				withExamples("goobers claims list", "goobers claims list --stale"),
			subcommand("claims release", "release", apicontract.ActionMaintenance, runClaimsRelease).
				withSynopsis(synopsisByID["claims release"]).
				withHelp("force-release a claim through the live daemon or claims.lock", claimsReleaseHelp).
				withExamples("goobers claims release 955", "goobers claims release --force 955"),
		).withHelp("inspect and force-release claim leases", claimsHelp),
		command("trace", apicontract.ActionReadOnlyNavigation, runTrace).
			withSynopsis(synopsisByID["trace"]).
			withHelp("show a run's journal events, follow a live run, or show transcripts", traceHelp).
			withExamples("goobers trace <run-id>", "goobers trace --follow <run-id>", "goobers trace --transcripts <run-id>"),
		commandWithSubcommands(
			"escalations",
			apicontract.ActionReadOnlyNavigation,
			runEscalations,
			subcommand("escalations show", "show", apicontract.ActionReadOnlyNavigation, runEscalationShow).
				withSynopsis(synopsisByID["escalations show"]).
				withHelp("show escalation cause + per-stage artifact timeline", escalationsShowHelp).
				withExamples("goobers escalations show <run-id>"),
		).
			withSynopsis(synopsisByID["escalations"]).
			withHelp("list escalated runs newest first", escalationsHelp).
			withExamples("goobers escalations", "goobers escalations --json"),
		groupCommand(
			"completion",
			runCompletion,
			subcommand(
				"completion bash",
				"bash",
				apicontract.ActionConfigTime,
				func(args []string, stdout, stderr io.Writer) int {
					return runCompletionScript(bashCompletion(), args, stdout, stderr)
				},
			).withHelp("generate a bash completion script", completionHelp),
			subcommand(
				"completion zsh",
				"zsh",
				apicontract.ActionConfigTime,
				func(args []string, stdout, stderr io.Writer) int {
					return runCompletionScript(zshCompletion(), args, stdout, stderr)
				},
			).withHelp("generate a zsh completion script", completionHelp),
			subcommand(
				"completion fish",
				"fish",
				apicontract.ActionConfigTime,
				func(args []string, stdout, stderr io.Writer) int {
					return runCompletionScript(fishCompletion(), args, stdout, stderr)
				},
			).withHelp("generate a fish completion script", completionHelp),
		).
			withSynopsis(synopsisByID["completion"]).
			withHelp("generate a shell completion script", completionHelp).
			withExamples("goobers completion bash", "goobers completion zsh"),
		command("__complete", apicontract.ActionConfigTime, func(args []string, stdout, _ io.Writer) int {
			return runCompletionCandidates(args, stdout)
		}),
		groupCommand(
			"telemetry",
			runTelemetry,
			subcommand("telemetry stats", "stats", apicontract.ActionReadOnlyNavigation, runTelemetryStats).
				withHelp("success rate and duration aggregates per workflow and stage", telemetryStatsHelp).
				withExamples("goobers telemetry stats", "goobers telemetry stats --json"),
			subcommand("telemetry errors", "errors", apicontract.ActionReadOnlyNavigation, runTelemetryErrors).
				withHelp("recent errors across runs, by class, with run/stage refs", telemetryErrorsHelp).
				withExamples("goobers telemetry errors", "goobers telemetry errors --limit=50"),
			subcommand("telemetry export", "export", apicontract.ActionReadOnlyNavigation, runTelemetryExport).
				withHelp("re-emit a span-start-time window from journaled OTLP/JSON", telemetryExportHelp).
				withExamples("goobers telemetry export --since=2026-07-01T00:00:00Z", "goobers telemetry export --since=2026-07-01T00:00:00Z --until=2026-07-02T00:00:00Z"),
			subcommand("telemetry prune", "prune", apicontract.ActionMaintenance, runTelemetryPrune).
				withHelp("remove terminal runs outside configured retention bounds", telemetryPruneHelp).
				withExamples("goobers telemetry prune --dry-run", "goobers telemetry prune"),
		).
			withSynopsis(synopsisByID["telemetry"]).
			withHelp("query, export, or prune run telemetry", telemetryHelp).
			withExamples("goobers telemetry stats", "goobers telemetry errors", "goobers telemetry export --since=2026-07-01T00:00:00Z", "goobers telemetry prune --dry-run"),
		groupCommand(
			"journal",
			runJournal,
			subcommand("journal redact", "redact", apicontract.ActionMaintenance, runJournalRedact).
				withSynopsis(synopsisByID["journal redact"]).
				withHelp("remove a leaked secret from a stored blob (SEC-041)", journalRedactHelp).
				withExamples("printf %s \"$LEAKED\" | goobers journal redact --run <id> --path inputs/creds.env --reason 'leak'"),
		).withHelp("the one sanctioned edit to the append-only journal", journalHelp),
		command("backlog-dedupe", apicontract.ActionWorkflowExecution, runBacklogDedupe).
			withSynopsis(synopsisByID["backlog-dedupe"]).
			withHelp("surface ranked duplicate candidates for curator judgment (a workflow stage)", backlogDedupeHelp).
			withExamples("goobers backlog-dedupe"),
		command("backlog-health", apicontract.ActionWorkflowExecution, runBacklogHealth).
			withSynopsis(synopsisByID["backlog-health"]).
			withHelp("snapshot ready-pool depth and age (a workflow stage)", backlogHealthHelp).
			withExamples("goobers backlog-health"),
		command("backlog-query", apicontract.ActionWorkflowExecution, runBacklogQuery).
			withSynopsis(synopsisByID["backlog-query"]).
			withHelp("query/claim one eligible backlog item (a workflow stage)", backlogQueryHelp).
			withExamples("goobers backlog-query", "goobers backlog-query --claim"),
		command("reconcile-branches", apicontract.ActionWorkflowExecution, runReconcileBranches).
			withSynopsis(synopsisByID["reconcile-branches"]).
			withHelp("report bounded stale goobers/* branch candidates (a workflow stage)", reconcileBranchesHelp).
			withExamples("goobers reconcile-branches", "goobers reconcile-branches --delete --max 5"),
		command("push-branch", apicontract.ActionWorkflowExecution, runPushBranch).
			withSynopsis(synopsisByID["push-branch"]).
			withHelp("push the worktree's checked-out branch to origin (a workflow stage)", pushBranchHelp).
			withExamples("goobers push-branch"),
		command("open-pr", apicontract.ActionWorkflowExecution, runOpenPR).
			withSynopsis(synopsisByID["open-pr"]).
			withHelp("open or update the run's PR (a workflow stage)", openPRHelp).
			withExamples("goobers open-pr"),
		command("issue-close-out", apicontract.ActionWorkflowExecution, runIssueCloseOut).
			withSynopsis(synopsisByID["issue-close-out"]).
			withHelp("comment + close out the claimed issue (a workflow stage)", issueCloseOutHelp).
			withExamples("goobers issue-close-out"),
		command("set-milestone", apicontract.ActionWorkflowExecution, runSetMilestone).
			withSynopsis(synopsisByID["set-milestone"]).
			withHelp("assign an existing milestone to an issue (a workflow stage)", setMilestoneHelp).
			withExamples("goobers set-milestone --item 1227 --milestone 22"),
		command("merge-pr", apicontract.ActionWorkflowExecution, runMergePR).
			withSynopsis(synopsisByID["merge-pr"]).
			withHelp("conjunctive auto-merge via direct-merge or merge-queue (a workflow stage)", mergePRHelp).
			withExamples("goobers merge-pr"),
		command("record-merge-refusal", apicontract.ActionWorkflowExecution, runRecordMergeRefusal).
			withSynopsis(synopsisByID["record-merge-refusal"]).
			withHelp("record a merge refusal and demote a persistently-stuck lander (a workflow stage)", recordMergeRefusalHelp).
			withExamples("goobers record-merge-refusal"),
		command("merge-queue-poll", apicontract.ActionWorkflowExecution, runMergeQueuePoll).
			withSynopsis(synopsisByID["merge-queue-poll"]).
			withHelp("watch an enqueued PR until merged or evicted (a workflow stage)", mergeQueuePollHelp).
			withExamples("goobers merge-queue-poll"),
		command("reconcile-post-merge", apicontract.ActionWorkflowExecution, runReconcilePostMerge).
			withSynopsis(synopsisByID["reconcile-post-merge"]).
			withHelp("reconcile late merge-queue merges (a workflow stage)", reconcilePostMergeHelp).
			withExamples("goobers reconcile-post-merge"),
		command("post-merge", apicontract.ActionWorkflowExecution, runPostMerge).
			withSynopsis(synopsisByID["post-merge"]).
			withHelp("post-merge fan-out + close the referenced issue (a workflow stage)", postMergeHelp).
			withExamples("goobers post-merge"),
		command("telemetry-query", apicontract.ActionWorkflowExecution, runTelemetryQuery).
			withSynopsis(synopsisByID["telemetry-query"]).
			withHelp("emit versioned candidate findings (a connector stage)", telemetryQueryHelp).
			withExamples("goobers telemetry-query --window 24h --format candidate-findings"),
		command("docs-churn", apicontract.ActionWorkflowExecution, runDocsChurn).
			withSynopsis(synopsisByID["docs-churn"]).
			withHelp("emit the docs-drift churn digest since the watermark (a connector stage)", docsChurnHelp).
			withExamples("goobers docs-churn --format churn-digest"),
		command("pr-select", apicontract.ActionWorkflowExecution, runPRSelect).
			withSynopsis(synopsisByID["pr-select"]).
			withHelp("select one eligible open PR for merge-review (a workflow stage)", prSelectHelp).
			withExamples("goobers pr-select"),
		command("gather-sibling-context", apicontract.ActionWorkflowExecution, runGatherSiblingContext).
			withSynopsis(synopsisByID["gather-sibling-context"]).
			withHelp("load other open PRs as review evidence (a workflow stage)", gatherSiblingContextHelp).
			withExamples("goobers gather-sibling-context"),
		command(gatherContextID, apicontract.ActionWorkflowExecution, runGatherImplementContext).
			withSynopsis(synopsisByID[gatherContextID]).
			withHelp("load first-pass implementation review and hot-file context (a workflow stage)", gatherImplementContextHelp).
			withExamples("goobers gather-implement-context"),
		command("apply-verdict", apicontract.ActionWorkflowExecution, runApplyVerdict).
			withSynopsis(synopsisByID["apply-verdict"]).
			withHelp("publish a merge-review verdict as a native review (a workflow stage)", applyVerdictHelp).
			withExamples("goobers apply-verdict"),
		command("elect-lander", apicontract.ActionWorkflowExecution, runElectLander).
			withSynopsis(synopsisByID["elect-lander"]).
			withHelp("elect the landing PR among a merge-review cohort (a workflow stage)", electLanderHelp).
			withExamples("goobers elect-lander"),
		command("update-behind-pr", apicontract.ActionWorkflowExecution, runUpdateBehindPR).
			withSynopsis(synopsisByID["update-behind-pr"]).
			withHelp("API-update a clean behind-base PR, else route to remediation (a workflow stage)", updateBehindPRHelp).
			withExamples("goobers update-behind-pr"),
		command("gather-pr-context", apicontract.ActionWorkflowExecution, runGatherPRContext).
			withSynopsis(synopsisByID["gather-pr-context"]).
			withHelp("pr-remediation entrypoint: select and load a PR's context (a workflow stage)", gatherPRContextHelp).
			withExamples("goobers gather-pr-context"),
		command("gather-review-threads", apicontract.ActionWorkflowExecution, runGatherReviewThreads).
			withSynopsis(synopsisByID["gather-review-threads"]).
			withHelp("add native reviews and anchored inline threads to a remediation brief (a workflow stage)", gatherReviewThreadsHelp).
			withExamples("goobers gather-review-threads"),
		command("gather-issue-context", apicontract.ActionWorkflowExecution, runGatherIssueContext).
			withSynopsis(synopsisByID["gather-issue-context"]).
			withHelp("add originating issue bodies to a remediation brief (a workflow stage)", gatherIssueContextHelp).
			withExamples("goobers gather-issue-context"),
		command("gather-ci-failures", apicontract.ActionWorkflowExecution, runGatherCIFailures).
			withSynopsis(synopsisByID["gather-ci-failures"]).
			withHelp("add failing CI diagnostics to a remediation brief (a workflow stage)", gatherCIFailuresHelp).
			withExamples("goobers gather-ci-failures"),
		command("rebase-pr", apicontract.ActionWorkflowExecution, runRebasePR).
			withSynopsis(synopsisByID["rebase-pr"]).
			withHelp("rebase-first, finding-driven remediation routing (a workflow stage)", rebasePRHelp).
			withExamples("goobers rebase-pr"),
		command("remediation-checkpoint", apicontract.ActionWorkflowExecution, runRemediationCheckpoint).
			withSynopsis(synopsisByID["remediation-checkpoint"]).
			withHelp("durable per-PR repass budget + same-diff escalation (a workflow stage)", remediationCheckpointHelp).
			withExamples("goobers remediation-checkpoint --budget 3"),
		command("push-remediated", apicontract.ActionWorkflowExecution, runPushRemediated).
			withSynopsis(synopsisByID["push-remediated"]).
			withHelp("force-push the remediated branch and clear needs-remediation (a workflow stage)", pushRemediatedHelp).
			withExamples("goobers push-remediated"),
		command("respond-to-findings", apicontract.ActionWorkflowExecution, runRespondToFindings).
			withSynopsis(synopsisByID["respond-to-findings"]).
			withHelp("post a validated per-finding remediation response to the claimed PR (a workflow stage)", respondToFindingsHelp).
			withExamples("goobers respond-to-findings"),
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
}

func command(
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	registration := aliasCommand(name, []string{name}, class, handler)
	if resultFile, ok := executor.ProviderStageResultFile(name); ok {
		registration.providerStage = true
		registration.resultFile = resultFile
	}
	return registration
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

// commandHelp resolves a command by its full invocation path (space-joined
// canonical names, e.g. "open-pr", "run abort", "claims list"). It is the
// lookup behind helpUsage, so a command's `-h` output is sourced from the same
// registry entry that defines it.
func commandHelp(id string) (cliCommand, bool) {
	return findCommandByPath(cliCommands, nil, id)
}

func findCommandByPath(commands []cliCommand, prefix []string, id string) (cliCommand, bool) {
	for _, command := range commands {
		if len(command.names) == 0 {
			continue
		}
		path := append(append([]string{}, prefix...), command.names[0])
		if strings.Join(path, " ") == id {
			return command, true
		}
		if sub, ok := findCommandByPath(command.subcommands, path, id); ok {
			return sub, true
		}
	}
	return cliCommand{}, false
}

// helpUsage returns a flag.FlagSet.Usage function that renders the registered
// long help for the command with the given invocation path to w. Handlers wire
// this in place of a bespoke inline help string so the registry is the single
// source of truth (#1095).
func helpUsage(w io.Writer, id string) func() {
	return func() {
		if command, ok := commandHelp(id); ok {
			pf(w, "%s", command.long)
		}
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

func (c cliCommand) dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if subcommand, ok := findCLICommandIn(c.subcommands, args[0]); ok {
			return subcommand.dispatch(args[1:], stdout, stderr)
		}
	}
	if c.providerStage {
		return runProviderStageCommand(c.names[0], c.resultFile, c.run, args, stdout, stderr)
	}
	return c.run(args, stdout, stderr)
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
