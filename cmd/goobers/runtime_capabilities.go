package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
)

type cliCommandHandler func([]string, io.Writer, io.Writer) int

var (
	cliFlagSetObserverMu sync.RWMutex
	cliFlagSetObserver   func(string, *flag.FlagSet)
)

func observeCLIFlagSet(id string, fs *flag.FlagSet) {
	cliFlagSetObserverMu.RLock()
	observer := cliFlagSetObserver
	cliFlagSetObserverMu.RUnlock()
	if observer != nil {
		observer(id, fs)
	}
}

func newCLIFlagSet(id string, errorHandling flag.ErrorHandling) *flag.FlagSet {
	fs := flag.NewFlagSet(id, errorHandling)
	observeCLIFlagSet(id, fs)
	return fs
}

type cliCommandTier uint8

const (
	cliTierAdvanced cliCommandTier = iota
	cliTierCore
	cliTierStage
)

type cliCommand struct {
	names            []string
	action           apicontract.SurfaceAction
	actionRegistered bool
	subcommands      []cliCommand
	run              cliCommandHandler
	providerStage    bool
	resultFile       string

	// Help metadata — the single source of truth for every rendered help
	// surface (#1095, CLI-1). Both the tiered usage views and each command's own
	// `-h` output derive from these fields; nothing hand-writes help text twice.
	// short is a one-line description; long is the full `-h` help body (verbatim,
	// trailing newline included); synopsis is the command-list entry; examples
	// are runnable invocations for generated man pages and the CLI reference.
	short    string
	long     string
	synopsis string
	examples []string
	tier     cliCommandTier
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
		coreAliasCommand(
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
		coreCommand("init", apicontract.ActionConfigTime, runInit).
			withSynopsis(synopsisByID["init"]).
			withHelp("scaffold an instance root", initHelp).
			withExamples("goobers init", "goobers init --template=quickstart ./tutorial", "goobers init --template=quickstart --source-tree ./tutorial-config --json", "goobers init --guided ./my-instance", "goobers init --demo ./demo"),
		coreCommand("connect", apicontract.ActionConfigTime, runConnect).
			withSynopsis(synopsisByID["connect"]).
			withHelp("connect an instance to your own GitHub repository", connectHelp).
			withExamples(
				"goobers connect acme/web ./my-instance",
				"goobers connect acme/web --token-env MY_GITHUB_TOKEN --seed ./my-instance",
				"goobers connect acme/web --json ./my-instance",
			),
		command("preflight", apicontract.ActionWorkflowExecution, runOnboardingPreflight).
			withSynopsis(synopsisByID["preflight"]).
			withHelp("check WSL full-isolation readiness and optionally hand off a command", wslPreflightHelp).
			withExamples("goobers preflight", "goobers preflight --distro Ubuntu-24.04", "goobers preflight --launch-wsl -- run implementation ."),
		groupCommand(
			"onboarding",
			runOnboarding,
			subcommand(
				"onboarding stub-agent-instructions",
				"stub-agent-instructions",
				apicontract.ActionConfigTime,
				runOnboardingStubAgentInstructions,
			).
				withHelp("install agent-instruction assets into a config source", stubAgentInstructionsHelp).
				withExamples(
					"goobers onboarding stub-agent-instructions --source-tree ./config-repo --harness copilot --json",
				),
			subcommand("onboarding stub-sample", "stub-sample", apicontract.ActionConfigTime, runOnboardingStubSample).
				withHelp("materialize and optionally seed the disposable Getting Started target", stubSampleHelp).
				withExamples(
					"goobers onboarding stub-sample --destination ./getting-started-task-api --json",
					"goobers onboarding stub-sample --destination ./getting-started-task-api --work-tracking my-org/tutorial",
				),
		).
			withSynopsis(synopsisByID["onboarding"]).
			withHelp("run non-interactive onboarding actions", onboardingHelp).
			withExamples(
				"goobers onboarding stub-agent-instructions --source-tree ./config-repo --harness copilot --json",
				"goobers onboarding stub-sample --destination ./getting-started-task-api --json",
			),
		coreGroupCommand(
			"examples",
			runExamples,
			subcommand("examples list", "list", apicontract.ActionReadOnlyNavigation, runExamplesList).
				withHelp("list canonical embedded workflow examples", examplesListHelp).
				withExamples("goobers examples list"),
			subcommand("examples show", "show", apicontract.ActionReadOnlyNavigation, runExamplesShow).
				withHelp("print a canonical embedded workflow example", examplesShowHelp).
				withExamples("goobers examples show implementation"),
		).
			withSynopsis(synopsisByID["examples"]).
			withHelp("browse canonical workflow examples embedded in the binary", examplesHelp).
			withExamples("goobers examples list", "goobers examples show implementation"),
		coreGroupCommand(
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
			subcommand(
				"scaffold gaggle",
				"gaggle",
				apicontract.ActionConfigTime,
				runScaffoldGaggle,
			).withHelp("scaffold a gaggle, or rename one with --from", scaffoldGaggleHelp),
		).
			withSynopsis(synopsisByID["scaffold"]).
			withHelp("scaffold a goober, workflow, or gaggle", scaffoldHelp).
			withExamples("goobers scaffold goober my-coder", "goobers scaffold workflow my-flow", "goobers scaffold gaggle ledger --from example"),
		groupCommand(
			"agent-kit",
			runAgentKit,
			subcommand("agent-kit install", "install", apicontract.ActionConfigTime, runAgentKitInstall).
				withHelp("install the release-matched agent toolkit", agentKitInstallHelp).
				withExamples("goobers agent-kit install --harness copilot ./config-repo"),
			subcommand("agent-kit check", "check", apicontract.ActionReadOnlyNavigation, runAgentKitCheck).
				withHelp("report agent toolkit version and drift", agentKitCheckHelp).
				withExamples("goobers agent-kit check ./config-repo"),
			subcommand("agent-kit update", "update", apicontract.ActionConfigTime, runAgentKitUpdate).
				withHelp("review or explicitly apply an agent toolkit update", agentKitUpdateHelp).
				withExamples("goobers agent-kit update ./config-repo", "goobers agent-kit update --write ./config-repo"),
		).
			withSynopsis(synopsisByID["agent-kit"]).
			withHelp("install, inspect, or update the release-matched agent toolkit", agentKitHelp).
			withExamples("goobers agent-kit install --harness generic ./config-repo", "goobers agent-kit check ./config-repo"),
		coreCommand("validate", apicontract.ActionConfigTime, runValidate).
			withSynopsis(synopsisByID["validate"]).
			withHelp("validate an instance or checked-in config source tree", validateHelp).
			withExamples("goobers validate", "goobers validate --json", "goobers validate --check-harness --check-repos"),
		command("lint", apicontract.ActionConfigTime, runLint).
			withSynopsis(synopsisByID["lint"]).
			withHelp("lint config via the single authoritative validation engine (alias for validate)", lintHelp).
			withExamples("goobers lint", "goobers lint --json", "goobers lint --check-harness --check-repos"),
		command("fix", apicontract.ActionConfigTime, runFix).
			withSynopsis(synopsisByID["fix"]).
			withHelp("mechanically migrate workflows to a target dslVersion, one step at a time (DVL-6)", fixHelp).
			withExamples("goobers fix --to 2.0", "goobers fix --to 2.0 --write ./instance"),
		command("doctor", apicontract.ActionReadOnlyNavigation, runDoctor).
			withSynopsis(synopsisByID["doctor"]).
			withHelp("preflight a Kubernetes cluster, repository forge policy, or Windows antivirus exclusions", doctorHelp).
			withExamples("goobers doctor --k8s", "goobers doctor --k8s --report json --oidc-issuer https://login.example.com/tenant/v2.0", "goobers doctor --av-exclusions --report json ./instance"),
		command("netpol-render", apicontract.ActionConfigTime, runNetpolRender).
			withSynopsis(synopsisByID["netpol-render"]).
			withHelp("render per-runner-class NetworkPolicy reference manifests from the runners: inventory", netpolRenderHelp).
			withExamples(
				"goobers netpol-render --out ./deploy/netpol",
				"goobers netpol-render --out ./deploy/netpol --write-baseline",
				"goobers netpol-render --out ./deploy/netpol --check",
			),
		groupCommand(
			"config",
			runConfig,
			subcommand("config diff", "diff", apicontract.ActionConfigTime, runConfigDiff).
				withHelp("compare active workflows with canonical definitions", configDiffHelp).
				withExamples("goobers config diff ./instance", "goobers config diff --against ./reference-workflows ./instance"),
			subcommand("config materialize", "materialize", apicontract.ActionConfigTime, runConfigMaterialize).
				withHelp("apply the recorded checked-in source to the runtime instance", configMaterializeHelp).
				withExamples("goobers config materialize", "goobers config materialize ./instance"),
			subcommand("config show", "show", apicontract.ActionReadOnlyNavigation, runConfigShow).
				withHelp("render the effective instance config (secrets redacted)", configShowHelp).
				withExamples("goobers config show", "goobers config show --json"),
		).
			withSynopsis(synopsisByID["config"]).
			withHelp("inspect, materialize, and compare instance configuration", configHelp).
			withExamples("goobers config show", "goobers config materialize ./instance", "goobers config diff ./instance"),
		groupCommand(
			"speech",
			runSpeech,
			subcommand("speech preflight", "preflight", apicontract.ActionReadOnlyNavigation, runSpeechPreflight).
				withHelp("check the configured local speech engine without emitting sound", speechPreflightHelp).
				withExamples("goobers speech preflight", "goobers speech preflight --json ./instance"),
			subcommand("speech test", "test", apicontract.ActionMaintenance, runSpeechTest).
				withHelp("speak the fixed local readiness phrase", speechTestHelp).
				withExamples("goobers speech test", "goobers speech test --json ./instance"),
		).
			withSynopsis(synopsisByID["speech"]).
			withHelp("preflight and test local speech notifications", speechHelp).
			withExamples("goobers speech preflight", "goobers speech test"),
		groupCommand(
			"fleet",
			runFleet,
			subcommand("fleet join", "join", apicontract.ActionConfigTime, runFleetJoin).
				withHelp("discover and enroll this instance with a Fleet service", fleetJoinHelp).
				withExamples("goobers fleet join --url https://fleet.example", "goobers fleet join --url https://fleet.example --enrollment-token-file ./grant.txt --grant-local-admin"),
			subcommand("fleet status", "status", apicontract.ActionReadOnlyNavigation, runFleetStatus).
				withHelp("show durable Fleet registration and connection state", fleetStatusHelp).
				withExamples("goobers fleet status", "goobers fleet status --json"),
			subcommand("fleet leave", "leave", apicontract.ActionMaintenance, runFleetLeave).
				withHelp("remove this instance's Fleet association and protected secrets", fleetLeaveHelp).
				withExamples("goobers fleet leave"),
		).
			withSynopsis(synopsisByID["fleet"]).
			withHelp("associate this instance with a Fleet service", fleetHelp).
			withExamples("goobers fleet join --url https://fleet.example", "goobers fleet status", "goobers fleet leave"),
		coreCommand("up", apicontract.ActionDaemonLifecycle, runUp).
			withSynopsis(synopsisByID["up"]).
			withHelp("run the daemon (scheduler + runner + loopback HTTP API)", upHelp).
			withExamples("goobers up", "goobers up --quiet --notify=all"),
		coreCommand("down", apicontract.ActionDaemonLifecycle, runDown).
			withSynopsis(synopsisByID["down"]).
			withHelp("request a live daemon's graceful drain-shutdown from a separate terminal", downHelp).
			withExamples("goobers down", "goobers down ./instance"),
		command("apply", apicontract.ActionDaemonLifecycle, runApply).
			withSynopsis(synopsisByID["apply"]).
			withHelp("reconcile a live daemon's workflow definitions now", applyHelp).
			withExamples("goobers apply", "goobers apply ./instance"),
		command("self-update", apicontract.ActionDaemonLifecycle, runSelfUpdate).
			withSynopsis(synopsisByID["self-update"]).
			withHelp("stage and request a supervised binary update", selfUpdateHelp).
			withExamples("goobers self-update --policy on-release", "goobers self-update --policy manual --target v0.1.0"),
		command("__service-supervise", apicontract.ActionDaemonLifecycle, runServiceSupervise),
		coreGroupCommand(
			"service",
			runService,
			subcommand("service install", "install", apicontract.ActionDaemonLifecycle, runServiceInstall).
				withHelp("install, enable, and start the supervised daemon", serviceInstallHelp).
				withExamples("goobers service install", "goobers service install ./instance"),
			subcommand("service uninstall", "uninstall", apicontract.ActionDaemonLifecycle, runServiceUninstall).
				withHelp("gracefully stop and remove the supervised daemon", serviceUninstallHelp).
				withExamples("goobers service uninstall", "goobers service uninstall ./instance"),
			subcommand("service stop", "stop", apicontract.ActionDaemonLifecycle, runServiceStop).
				withHelp("halt the running daemon without disabling or removing it", serviceStopHelp).
				withExamples("goobers service stop", "goobers service stop ./instance"),
			subcommand("service start", "start", apicontract.ActionDaemonLifecycle, runServiceStart).
				withHelp("resume an installed-but-stopped daemon", serviceStartHelp).
				withExamples("goobers service start", "goobers service start ./instance"),
			subcommand("service status", "status", apicontract.ActionReadOnlyNavigation, runServiceStatus).
				withHelp("report whether the supervised daemon is installed and running", serviceStatusHelp).
				withExamples("goobers service status", "goobers service status --json"),
		).
			withSynopsis(synopsisByID["service"]).
			withHelp("install and manage the platform-supervised daemon", serviceHelp).
			withExamples("goobers service install", "goobers service status", "goobers service uninstall"),
		command("engine-start", apicontract.ActionDaemonLifecycle, runEngineStart).
			withSynopsis(synopsisByID["engine-start"]).
			withHelp("dispatch one run onto the tier-3 engine via Temporal (experimental)", engineStartHelp).
			withExamples("goobers engine-start default-implement"),
		command("engine-queues", apicontract.ActionReadOnlyNavigation, runEngineQueues).
			withSynopsis(synopsisByID["engine-queues"]).
			withHelp("report which workers poll this instance's engine and dispatch task queues (experimental)", engineQueuesHelp).
			withExamples("goobers engine-queues", "goobers engine-queues --json"),
		command("engine-project", apicontract.ActionDaemonLifecycle, runEngineProject).
			withSynopsis(synopsisByID["engine-project"]).
			withHelp("write a completed engine run's journal into the instance (experimental)", engineProjectHelp).
			withExamples("goobers engine-project --gaggle example <run-id>"),
		command("worker", apicontract.ActionDaemonLifecycle, runWorker).
			withSynopsis(synopsisByID["worker"]).
			withHelp("host a Temporal engine worker: task queues, graceful drain, versioned identity (tier-3, experimental)", workerHelp).
			withExamples("goobers worker", "goobers worker --task-queue goobers-engine --drain-timeout 60s"),
		coreCommand("dashboard", apicontract.ActionReadOnlyNavigation, runDashboard).
			withSynopsis(synopsisByID["dashboard"]).
			withHelp("serve and open the local operations portal", fmt.Sprintf(dashboardHelp, defaultDashboardPort)).
			withExamples("goobers dashboard", "goobers dashboard --port=auto --no-open"),
		// Read-only-navigation like dashboard: the server itself only navigates —
		// every guided write action is a user-invoked CLI subprocess that carries
		// its own action class.
		coreCommand("getting-started", apicontract.ActionReadOnlyNavigation, runGettingStarted).
			withSynopsis(synopsisByID["getting-started"]).
			withHelp("serve and open the guided portal Getting Started walkthrough", fmt.Sprintf(gettingStartedHelp, defaultDashboardPort)).
			withExamples("goobers getting-started", "goobers getting-started --no-open --workdir ~/goobers-tutorial"),
		coreCommandWithSubcommands(
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
			withExamples("goobers run default-implement", "goobers run --gaggle example default-implement", "goobers run example/default-implement --no-wait"),
		runtimeCommand("approve", "approve", runApprove).
			withSynopsis(synopsisByID["approve"]).
			withHelp("approve a paused or escalated gate", approveHelp).
			withExamples("goobers approve <run-id> <gate>"),
		runtimeCommand("override", "override", runOverride).
			withSynopsis(synopsisByID["override"]).
			withHelp("override a nondeterministic gate with a rationale", overrideHelp).
			withExamples(`goobers override --rationale="accepted risk" <run-id> <gate>`),
		runtimeCommand("rerun-stage", "rerun", runRerunStage).
			withSynopsis(synopsisByID["rerun-stage"]).
			withHelp("rerun a stage with a recorded instruction addendum", rerunStageHelp).
			withExamples(`goobers rerun-stage --addendum="use the parser seam" <run-id> <stage>`),
		command(detachedRunWorkerCommand, apicontract.ActionWorkflowExecution, runDetachedWorker),
		command(dispatcher.DispatchExecCommand, apicontract.ActionWorkflowExecution, runDispatchExec),
		command(demoProviderCommand, apicontract.ActionWorkflowExecution, runDemoProvider),
		command(wslNetworkPreflightCommand, apicontract.ActionConfigTime, runWSLNetworkPreflight),
		coreCommand("signal", apicontract.ActionWorkflowExecution, runSignal).
			withSynopsis(synopsisByID["signal"]).
			withHelp("fire an external signal to subscribed workflows", signalHelp).
			withExamples("goobers signal deploy-approved"),
		coreGroupCommand(
			"workflow",
			runWorkflow,
			coreSubcommand("workflow show", "show", apicontract.ActionReadOnlyNavigation, runWorkflowShow).
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
		coreCommand("status", apicontract.ActionReadOnlyNavigation, runStatus).
			withSynopsis(synopsisByID["status"]).
			withHelp("validate config, show warnings, list runs, report daemon health, or list live agentic stages", statusHelp).
			withExamples("goobers status", "goobers status --daemon", "goobers status --watch", "goobers status --agents", "goobers status --agents --json"),
		coreCommand("stats", apicontract.ActionReadOnlyNavigation, runStats).
			withSynopsis(synopsisByID["stats"]).
			withHelp("show the instance lifetime summary card", statsHelp).
			withExamples("goobers stats", "goobers stats --since 24h --json"),
		command("features", apicontract.ActionReadOnlyNavigation, runFeatures).
			withSynopsis(synopsisByID["features"]).
			withHelp("list the workflow-DSL features this build supports", featuresHelp).
			withExamples("goobers features", "goobers features --json --dsl-version 2.0", "goobers features --used"),
		command("schema", apicontract.ActionReadOnlyNavigation, runSchema).
			withSynopsis(synopsisByID["schema"]).
			withHelp("emit a JSON Schema embedded in this build", schemaHelp).
			withExamples("goobers schema --list", "goobers schema workflow", "goobers schema --human goober"),
		command("explain", apicontract.ActionReadOnlyNavigation, runExplain).
			withSynopsis(synopsisByID["explain"]).
			withHelp("project field facts from an embedded JSON Schema", explainHelp).
			withExamples("goobers explain goober.spec.capabilities", "goobers explain --human workflow.spec.gates[].evaluator"),
		command("reset-rate-limit", apicontract.ActionMaintenance, runResetRateLimit).
			withSynopsis(synopsisByID["reset-rate-limit"]).
			withHelp("clear the hourly run-rate budget without deleting runs/", resetRateLimitHelp).
			withExamples("goobers reset-rate-limit"),
		groupCommand(
			"workspace",
			runWorkspace,
			subcommand("workspace reset", "reset", apicontract.ActionMaintenance, runWorkspaceReset).
				withHelp("tear down and re-materialize a pinned repository workspace", workspaceResetHelp).
				withExamples("goobers workspace reset my-repo", "goobers workspace reset acme/my-repo ./instance"),
		).
			withSynopsis(synopsisByID["workspace"]).
			withHelp("explicitly recover pinned repository workspaces", workspaceHelp).
			withExamples("goobers workspace reset my-repo"),
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
		coreCommand("trace", apicontract.ActionReadOnlyNavigation, runTrace).
			withSynopsis(synopsisByID["trace"]).
			withHelp("show a run's journal events or review verdicts, follow a live run, or show transcripts", traceHelp).
			withExamples("goobers trace <run-id>", "goobers trace --summary <run-id>", "goobers trace --verdicts <run-id>", "goobers trace --follow <run-id>", "goobers trace --transcripts <run-id>"),
		groupCommand(
			"e2e",
			runE2E,
			subcommand("e2e verify", "verify", apicontract.ActionReadOnlyNavigation, runE2EVerify).
				withSynopsis(synopsisByID["e2e verify"]).
				withHelp("verify the Goobernetes S1-S9 e2e proof harness against one completed run's recorded data", e2eVerifyHelp).
				withExamples("goobers e2e verify --run <run-id>", "goobers e2e verify --run <run-id> --expected topology.json --out bundle.json", "goobers e2e verify --print-runner-class network:allowlist"),
			subcommand("e2e kill-inject", "kill-inject", apicontract.ActionMaintenance, runE2EKillInject).
				withSynopsis(synopsisByID["e2e kill-inject"]).
				withHelp("perform one live S6 kill-matrix cell (pod-kill) against a real cluster", e2eKillInjectHelp).
				withExamples("goobers e2e kill-inject --run <run-id> --stage probe-builtin --stage-class builtin --namespace gaggle-goobers"),
		).withHelp("check the Goobernetes distributed e2e proof harness's assertions against a recorded run", e2eHelp),
		coreCommandWithSubcommands(
			"escalations",
			apicontract.ActionReadOnlyNavigation,
			runEscalations,
			subcommand("escalations show", "show", apicontract.ActionReadOnlyNavigation, runEscalationShow).
				withSynopsis(synopsisByID["escalations show"]).
				withHelp("show escalation cause, verdict, and per-stage artifact timeline", escalationsShowHelp).
				withExamples("goobers escalations show <run-id>", "goobers escalations show --include-verdict <run-id>"),
		).
			withSynopsis(synopsisByID["escalations"]).
			withHelp("list escalated runs newest first", escalationsHelp).
			withExamples("goobers escalations", "goobers escalations --json"),
		coreGroupCommand(
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
			subcommand(
				"completion powershell",
				"powershell",
				apicontract.ActionConfigTime,
				func(args []string, stdout, stderr io.Writer) int {
					return runCompletionScript(powershellCompletion(), args, stdout, stderr)
				},
			).withHelp("generate a PowerShell completion script", completionHelp),
		).
			withSynopsis(synopsisByID["completion"]).
			withHelp("generate a shell completion script", completionHelp).
			withExamples("goobers completion bash", "goobers completion zsh", "goobers completion powershell"),
		command("__complete", apicontract.ActionConfigTime, func(args []string, stdout, _ io.Writer) int {
			return runCompletionCandidates(args, stdout)
		}),
		command("__generate-docs", apicontract.ActionConfigTime, runGenerateDocs),
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
			subcommand("telemetry prune-orphans", "prune-orphans", apicontract.ActionMaintenance, runTelemetryPruneOrphans).
				withHelp("report or delete old orphan and unfinished run directories", telemetryPruneOrphansHelp).
				withExamples("goobers telemetry prune-orphans", "goobers telemetry prune-orphans --delete"),
			subcommand("telemetry compact", "compact", apicontract.ActionMaintenance, runTelemetryCompact).
				withHelp("drop aged scheduler journal/rollup rows and reclaim disk (VACUUM)", telemetryCompactHelp).
				withExamples("goobers telemetry compact --dry-run", "goobers telemetry compact"),
		).
			withSynopsis(synopsisByID["telemetry"]).
			withHelp("query, export, prune, or compact run telemetry", telemetryHelp).
			withExamples("goobers telemetry stats", "goobers telemetry errors", "goobers telemetry export --since=2026-07-01T00:00:00Z", "goobers telemetry prune --dry-run"),
		groupCommand(
			"journal",
			runJournal,
			subcommand("journal redact", "redact", apicontract.ActionMaintenance, runJournalRedact).
				withSynopsis(synopsisByID["journal redact"]).
				withHelp("remove a leaked secret from a stored blob (SEC-041)", journalRedactHelp).
				withExamples("printf %s \"$LEAKED\" | goobers journal redact --run <id> --path inputs/creds.env --reason 'leak'"),
		).withHelp("the one sanctioned edit to the append-only journal", journalHelp),
		stageCommand("backlog-dedupe", apicontract.ActionWorkflowExecution, runBacklogDedupe).
			withSynopsis(synopsisByID["backlog-dedupe"]).
			withHelp("surface ranked duplicate candidates for curator judgment (a workflow stage)", backlogDedupeHelp).
			withExamples("goobers backlog-dedupe"),
		stageCommand("backlog-assignment", apicontract.ActionWorkflowExecution, runBacklogAssignment).
			withSynopsis(synopsisByID["backlog-assignment"]).
			withHelp("assign eligible backlog items from a configured roster (a workflow stage)", backlogAssignmentHelp).
			withExamples("goobers backlog-assignment"),
		stageCommand("backlog-health", apicontract.ActionWorkflowExecution, runBacklogHealth).
			withSynopsis(synopsisByID["backlog-health"]).
			withHelp("snapshot ready-pool depth and age (a workflow stage)", backlogHealthHelp).
			withExamples("goobers backlog-health"),
		stageCommand("backlog-query", apicontract.ActionWorkflowExecution, runBacklogQuery).
			withSynopsis(synopsisByID["backlog-query"]).
			withHelp("query/claim one eligible backlog item (a workflow stage)", backlogQueryHelp).
			withExamples("goobers backlog-query", "goobers backlog-query --claim"),
		stageCommand("select-source", apicontract.ActionWorkflowExecution, runSelectSource).
			withSynopsis(synopsisByID["select-source"]).
			withHelp("select and claim an unconsumed L6 decomposition disposition (a workflow stage)", selectSourceHelp).
			withExamples("goobers select-source"),
		stageCommand("validate-plan", apicontract.ActionWorkflowExecution, runValidatePlan).
			withSynopsis(synopsisByID["validate-plan"]).
			withHelp("validate a decomposition plan against its selector artifact and the live parent (a workflow stage)", validatePlanHelp).
			withExamples("goobers validate-plan"),
		stageCommand("publish-batch", apicontract.ActionWorkflowExecution, runPublishBatch).
			withSynopsis(synopsisByID["publish-batch"]).
			withHelp("publish a verified decomposition batch behind one eligibility barrier (a workflow stage)", publishBatchHelp).
			withExamples("goobers publish-batch"),
		stageCommand("file-issues", apicontract.ActionWorkflowExecution, runFileIssues).
			withSynopsis(synopsisByID["file-issues"]).
			withHelp("file a validated nominations artifact as deduped, budgeted issues (a workflow stage)", fileIssuesHelp).
			withExamples("goobers file-issues --check", "goobers file-issues"),
		stageCommand("reconcile-branches", apicontract.ActionWorkflowExecution, runReconcileBranches).
			withSynopsis(synopsisByID["reconcile-branches"]).
			withHelp("report bounded stale goobers/* branch candidates (a workflow stage)", reconcileBranchesHelp).
			withExamples("goobers reconcile-branches", "goobers reconcile-branches --delete --max 5"),
		stageCommand("push-branch", apicontract.ActionWorkflowExecution, runPushBranch).
			withSynopsis(synopsisByID["push-branch"]).
			withHelp("push the worktree's checked-out branch to origin (a workflow stage)", pushBranchHelp).
			withExamples("goobers push-branch"),
		stageCommand("check-fail-first", apicontract.ActionWorkflowExecution, runCheckFailFirst).
			withSynopsis(synopsisByID["check-fail-first"]).
			withHelp("enforce fail-first evidence for a new workflow gate (a workflow stage)", checkFailFirstHelp).
			withExamples("goobers check-fail-first"),
		stageCommand("open-pr", apicontract.ActionWorkflowExecution, runOpenPR).
			withSynopsis(synopsisByID["open-pr"]).
			withHelp("open or update the run's PR (a workflow stage)", openPRHelp).
			withExamples("goobers open-pr"),
		stageCommand("report-pr-status", apicontract.ActionWorkflowExecution, runReportPRStatus).
			withSynopsis(synopsisByID["report-pr-status"]).
			withHelp("publish goobers' verdict + CI evidence as a policy-gate-able PR status (a workflow stage)", reportPRStatusHelp).
			withExamples("goobers report-pr-status"),
		stageCommand("gate-removal-guard", apicontract.ActionWorkflowExecution, runGateRemovalGuard).
			withSynopsis(synopsisByID["gate-removal-guard"]).
			withHelp("block a tutor run that removes/loosens its own flagged gate without proof (a workflow stage)", gateRemovalGuardHelp).
			withExamples("goobers gate-removal-guard"),
		stageCommand("issue-close-out", apicontract.ActionWorkflowExecution, runIssueCloseOut).
			withSynopsis(synopsisByID["issue-close-out"]).
			withHelp("comment + close out the claimed issue (a workflow stage)", issueCloseOutHelp).
			withExamples("goobers issue-close-out"),
		stageCommand("set-milestone", apicontract.ActionWorkflowExecution, runSetMilestone).
			withSynopsis(synopsisByID["set-milestone"]).
			withHelp("assign an existing milestone to an issue (a workflow stage)", setMilestoneHelp).
			withExamples("goobers set-milestone --item 1227 --milestone 22"),
		stageCommand("merge-pr", apicontract.ActionWorkflowExecution, runMergePR).
			withSynopsis(synopsisByID["merge-pr"]).
			withHelp("conjunctive auto-merge via direct-merge or merge-queue (a workflow stage)", mergePRHelp).
			withExamples("goobers merge-pr"),
		stageCommand("record-merge-refusal", apicontract.ActionWorkflowExecution, runRecordMergeRefusal).
			withSynopsis(synopsisByID["record-merge-refusal"]).
			withHelp("record a merge refusal and demote a persistently-stuck lander (a workflow stage)", recordMergeRefusalHelp).
			withExamples("goobers record-merge-refusal"),
		stageCommand("merge-queue-poll", apicontract.ActionWorkflowExecution, runMergeQueuePoll).
			withSynopsis(synopsisByID["merge-queue-poll"]).
			withHelp("watch an enqueued PR until merged, evicted, timed out, or opted out (a workflow stage)", mergeQueuePollHelp).
			withExamples("goobers merge-queue-poll"),
		stageCommand("reconcile-post-merge", apicontract.ActionWorkflowExecution, runReconcilePostMerge).
			withSynopsis(synopsisByID["reconcile-post-merge"]).
			withHelp("reconcile late merge-queue merges (a workflow stage)", reconcilePostMergeHelp).
			withExamples("goobers reconcile-post-merge"),
		stageCommand("post-merge", apicontract.ActionWorkflowExecution, runPostMerge).
			withSynopsis(synopsisByID["post-merge"]).
			withHelp("post-merge fan-out + close the referenced issue (a workflow stage)", postMergeHelp).
			withExamples("goobers post-merge"),
		stageCommand("telemetry-query", apicontract.ActionWorkflowExecution, runTelemetryQuery).
			withSynopsis(synopsisByID["telemetry-query"]).
			withHelp("emit versioned candidate findings (a connector stage)", telemetryQueryHelp).
			withExamples("goobers telemetry-query --window 24h --format candidate-findings"),
		stageCommand("docs-churn", apicontract.ActionWorkflowExecution, runDocsChurn).
			withSynopsis(synopsisByID["docs-churn"]).
			withHelp("emit the docs-drift churn digest since the watermark (a connector stage)", docsChurnHelp).
			withExamples("goobers docs-churn --format churn-digest"),
		stageCommand("ios-simulator-test", apicontract.ActionWorkflowExecution, runIOSSimulatorTest).
			withSynopsis(synopsisByID["ios-simulator-test"]).
			withHelp("run XCUITest on an iOS simulator and parse its xcresult (a workflow stage)", iosSimulatorTestHelp).
			withExamples("goobers ios-simulator-test --project App.xcodeproj --scheme AppUITests"),
		stageCommand("pr-select", apicontract.ActionWorkflowExecution, runPRSelect).
			withSynopsis(synopsisByID["pr-select"]).
			withHelp("select one managed or advisory open PR for merge-review (a workflow stage)", prSelectHelp).
			withExamples("goobers pr-select"),
		stageCommand("check-issue-staleness", apicontract.ActionWorkflowExecution, runCheckIssueStaleness).
			withSynopsis(synopsisByID["check-issue-staleness"]).
			withHelp("route a PR to remediation if its linked issue changed since implementation began (a workflow stage)", checkIssueStalenessHelp).
			withExamples("goobers check-issue-staleness"),
		stageCommand("gather-sibling-context", apicontract.ActionWorkflowExecution, runGatherSiblingContext).
			withSynopsis(synopsisByID["gather-sibling-context"]).
			withHelp("load other open PRs as review evidence (a workflow stage)", gatherSiblingContextHelp).
			withExamples("goobers gather-sibling-context"),
		stageCommand(gatherContextID, apicontract.ActionWorkflowExecution, runGatherImplementContext).
			withSynopsis(synopsisByID[gatherContextID]).
			withHelp("load first-pass implementation review and hot-file context (a workflow stage)", gatherImplementContextHelp).
			withExamples("goobers gather-implement-context"),
		stageCommand("apply-verdict", apicontract.ActionWorkflowExecution, runApplyVerdict).
			withSynopsis(synopsisByID["apply-verdict"]).
			withHelp("publish a managed or advisory merge-review verdict (a workflow stage)", applyVerdictHelp).
			withExamples("goobers apply-verdict"),
		stageCommand("elect-lander", apicontract.ActionWorkflowExecution, runElectLander).
			withSynopsis(synopsisByID["elect-lander"]).
			withHelp("elect the landing PR among a merge-review cohort (a workflow stage)", electLanderHelp).
			withExamples("goobers elect-lander"),
		stageCommand("update-behind-pr", apicontract.ActionWorkflowExecution, runUpdateBehindPR).
			withSynopsis(synopsisByID["update-behind-pr"]).
			withHelp("API-update a clean behind-base PR, else route to remediation (a workflow stage)", updateBehindPRHelp).
			withExamples("goobers update-behind-pr"),
		stageCommand("pr-claim", apicontract.ActionWorkflowExecution, runPRRemediationLifecycle).
			withSynopsis(synopsisByID["pr-claim"]).
			withHelp("check PR liveness or release its remediation claim (a workflow stage)", prRemediationLifecycleHelp).
			withExamples("goobers pr-claim", "goobers pr-claim --release"),
		stageCommand("gather-pr-context", apicontract.ActionWorkflowExecution, runGatherPRContext).
			withSynopsis(synopsisByID["gather-pr-context"]).
			withHelp("pr-remediation entrypoint: select and load a PR's context (a workflow stage)", gatherPRContextHelp).
			withExamples("goobers gather-pr-context"),
		stageCommand("gather-review-threads", apicontract.ActionWorkflowExecution, runGatherReviewThreads).
			withSynopsis(synopsisByID["gather-review-threads"]).
			withHelp("add native reviews and anchored inline threads to a remediation brief (a workflow stage)", gatherReviewThreadsHelp).
			withExamples("goobers gather-review-threads"),
		stageCommand("resolve-review-threads", apicontract.ActionWorkflowExecution, runResolveReviewThreads).
			withSynopsis(synopsisByID["resolve-review-threads"]).
			withHelp("reply to and resolve remediated native review threads (a workflow stage)", resolveReviewThreadsHelp).
			withExamples("goobers resolve-review-threads"),
		stageCommand("gather-issue-context", apicontract.ActionWorkflowExecution, runGatherIssueContext).
			withSynopsis(synopsisByID["gather-issue-context"]).
			withHelp("add originating issue bodies to a remediation brief (a workflow stage)", gatherIssueContextHelp).
			withExamples("goobers gather-issue-context"),
		stageCommand("pr-comment-watch", apicontract.ActionWorkflowExecution, runPRCommentWatch).
			withSynopsis(synopsisByID["pr-comment-watch"]).
			withHelp("label open goober PRs carrying unaddressed human comments (a workflow stage)", prCommentWatchHelp).
			withExamples("goobers pr-comment-watch"),
		stageCommand("gather-ci-failures", apicontract.ActionWorkflowExecution, runGatherCIFailures).
			withSynopsis(synopsisByID["gather-ci-failures"]).
			withHelp("add failing CI diagnostics to a remediation brief (a workflow stage)", gatherCIFailuresHelp).
			withExamples("goobers gather-ci-failures"),
		stageCommand("rebase-pr", apicontract.ActionWorkflowExecution, runRebasePR).
			withSynopsis(synopsisByID["rebase-pr"]).
			withHelp("rebase-first, finding-driven remediation routing (a workflow stage)", rebasePRHelp).
			withExamples("goobers rebase-pr"),
		stageCommand("remediation-checkpoint", apicontract.ActionWorkflowExecution, runRemediationCheckpoint).
			withSynopsis(synopsisByID["remediation-checkpoint"]).
			withHelp("durable per-cause attempt budgets + same-diff escalation (a workflow stage)", remediationCheckpointHelp).
			withExamples("goobers remediation-checkpoint"),
		stageCommand("push-remediated", apicontract.ActionWorkflowExecution, runPushRemediated).
			withSynopsis(synopsisByID["push-remediated"]).
			withHelp("force-push the remediated branch and clear needs-remediation (a workflow stage)", pushRemediatedHelp).
			withExamples("goobers push-remediated"),
		stageCommand("respond-to-findings", apicontract.ActionWorkflowExecution, runRespondToFindings).
			withSynopsis(synopsisByID["respond-to-findings"]).
			withHelp("post a validated per-finding remediation response to the claimed PR (a workflow stage)", respondToFindingsHelp).
			withExamples("goobers respond-to-findings"),
		stageCommand("mcp-io", apicontract.ActionWorkflowExecution, runMCPIO).
			withSynopsis(synopsisByID["mcp-io"]).
			withHelp("run the generic publish/read/list MCP server the harness spawns for a goober (a workflow stage)", mcpIOHelp).
			withExamples("goobers mcp-io --config .goobers/mcp-io/goobers-io-config.json"),
		coreAliasCommand(
			"help",
			[]string{"-h", "--help", "help"},
			apicontract.ActionReadOnlyNavigation,
			runHelpCommand,
		).withHelp("show command or concept help", helpHelp),
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

func coreCommand(
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	registration := command(name, class, handler)
	registration.tier = cliTierCore
	return registration
}

func stageCommand(
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	registration := command(name, class, handler)
	registration.tier = cliTierStage
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

func coreCommandWithSubcommands(
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
	subcommands ...cliCommand,
) cliCommand {
	registration := commandWithSubcommands(name, class, handler, subcommands...)
	registration.tier = cliTierCore
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

func coreGroupCommand(
	name string,
	handler cliCommandHandler,
	subcommands ...cliCommand,
) cliCommand {
	registration := groupCommand(name, handler, subcommands...)
	registration.tier = cliTierCore
	return registration
}

func subcommand(
	id string,
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	return aliasCommand(id, []string{name}, class, handler)
}

func coreSubcommand(
	id string,
	name string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	registration := subcommand(id, name, class, handler)
	registration.tier = cliTierCore
	return registration
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

func coreAliasCommand(
	id string,
	names []string,
	class apicontract.ActionClass,
	handler cliCommandHandler,
) cliCommand {
	registration := aliasCommand(id, names, class, handler)
	registration.tier = cliTierCore
	return registration
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
