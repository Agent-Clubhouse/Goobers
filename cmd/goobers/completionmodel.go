package main

import (
	"strings"

	"github.com/goobers/goobers/internal/instance"
)

// completionModel is the shell-agnostic description of the goobers CLI surface
// that every shell completion script is rendered from. Its command and
// subcommand tree is DERIVED from the cliCommand registry (the single source of
// truth #1095 established), so a command added to the registry automatically
// appears in completion — enforced by TestCompletionModelCoversRegistry, the
// CI parity guard that fails if the two ever diverge.
//
// Flags and argument-completion kinds are the two things the registry does not
// model (flags are declared imperatively in each handler's flag.FlagSet, and
// arg kinds only exist as the `__complete` backend's cases). They are annotated
// per command id in completionFlagSpecs / completionPositionalArgKinds below —
// one Go table shared by all three shells, replacing the three hand-maintained
// shell-script string constants that had already drifted from the registry
// (missing whole commands: blocked, reconcile-branches, docs-churn,
// push-remediated, elect-lander; and many missing/mismatched flags).
type completionModel struct {
	// commands are the user-facing top-level commands in registry order.
	commands []completionCommand
	// globalFlags are the top-level flag aliases that are not subcommands —
	// --version, -h, --help — carried from the version/help registry entries.
	globalFlags []string
}

// completionCommand is one node in the completion tree.
type completionCommand struct {
	name      string               // canonical leaf name (registry names[0])
	id        string               // full space-joined invocation path
	desc      string               // registry short help (renders as the zsh description)
	tier      cliCommandTier       // controls top-level progressive disclosure
	subs      []completionCommand  // nested subcommands, from the registry
	flags     []completionFlagSpec // annotated flags (completionFlagSpecs[id])
	argKind   string               // dynamic positional arg kind (workflows|runs|escalations|examples)
	argValues []string             // static positional argument candidates
}

// completionFlagSpec annotates one flag for completion. takesArg mirrors
// whether the underlying flag.Flag consumes a value (String/Int/Duration/Var)
// versus a bare bool; valueKind/values drive completion of that value.
type completionFlagSpec struct {
	name      string   // flag name without leading dashes
	takesArg  bool     // consumes a value
	valueKind string   // dynamic value completion kind (workflows|runs), or ""
	values    []string // static value completions, or nil
	desc      string   // short description (renders as the fish -d hint)
}

// completionPositionalArgKinds maps a command id to the dynamic completion kind
// for its next positional argument. These mirror the `__complete` backend's
// supported kinds (completionCandidates) and are the completion-specific
// knowledge the registry does not carry.
var completionPositionalArgKinds = map[string]string{
	"run":              "workflows",
	"run abort":        "runs",
	"trace":            "runs",
	"escalations show": "escalations",
	"examples show":    "examples",
	"workflow show":    "workflows",
}

var completionPositionalArgValues = map[string][]string{
	"help": append([]string{
		"all",
		"stages",
	}, helpConceptTopics...),
}

// completionFlagSpecs maps a command id to its completable flags. The set and
// spelling of each flag is sourced from that command's real flag.FlagSet (the
// authoritative definition); -h/--help is universal and added by the renderer,
// so it is not repeated here.
var completionFlagSpecs = map[string][]completionFlagSpec{
	"version": {
		{name: "json", desc: "Emit JSON"},
	},
	"versions": {
		{name: "json", desc: "Emit JSON"},
	},
	"init": {
		{name: "demo", desc: "Seed a credential-free runnable demo workflow"},
		{name: "insecure", desc: "Allow an unisolated Windows demo"},
		{name: "guided", desc: "Prompt for repository, credentials, and workflows"},
		{name: "template", takesArg: true, values: []string{instance.QuickstartTemplate}, desc: "Seed a named onboarding template"},
		{name: "source-tree", takesArg: true, desc: "Seed the template as a checked-in config source"},
		{name: "json", desc: "Emit the config-source action result as JSON"},
	},
	"connect": {
		{name: "token-env", takesArg: true, desc: "Repository token environment variable name"},
		{name: "seed", desc: "Ensure selector labels and one starter issue on the repository"},
		{name: "replace", desc: "Also rewrite entries already pointing at a real repository"},
		{name: "json", desc: "Emit the versioned onboarding action envelope"},
	},
	"preflight": {
		{name: "distro", takesArg: true, desc: "Select the WSL distro to check"},
		{name: "launch-wsl", desc: "Run the trailing Goobers command inside WSL"},
	},
	"onboarding stub-sample": {
		{name: "destination", takesArg: true, desc: "Sample destination"},
		{name: "work-tracking", takesArg: true, desc: "GitHub owner/repo to seed"},
		{name: "token-env", takesArg: true, desc: "Issue token environment variable"},
		{name: "force", desc: "Replace conflicting regular files"},
		{name: "json", desc: "Emit the versioned action envelope"},
	},
	"onboarding stub-agent-instructions": {
		{name: "source-tree", takesArg: true, desc: "Config source repository root"},
		{name: "harness", takesArg: true, values: []string{"copilot", "claude", "generic"}, desc: "Harness adapter"},
		{name: "json", desc: "Emit the versioned config-source action envelope"},
	},
	"scaffold goober": {
		{name: "force", desc: "Replace generated files that already exist"},
	},
	"scaffold workflow": {
		{name: "force", desc: "Replace generated files that already exist"},
	},
	"scaffold gaggle": {
		{name: "force", desc: "Replace generated files that already exist (only without --from)"},
		{name: "from", takesArg: true, desc: "Rename an existing gaggle to <name>"},
	},
	"agent-kit install": {
		{name: "harness", takesArg: true, values: []string{"copilot", "claude", "generic"}, desc: "Harness adapter"},
	},
	"agent-kit update": {
		{name: "dry-run", desc: "Show the update diff without writing"},
		{name: "write", desc: "Apply product-owned changes"},
		{name: "replace-modified", desc: "Acknowledge replacement of modified owned files"},
	},
	"validate": {
		{name: "json", desc: "Emit a versioned findings envelope"},
		{name: "github-annotations", desc: "Emit GitHub Actions file annotations"},
		{name: "check-harness", desc: "Verify referenced agent harnesses are installed and signed in"},
		{name: "check-repos", desc: "Verify target repositories are reachable"},
		{name: "source-tree", desc: "Validate a checked-in config source tree"},
		{name: "strict", desc: "Treat config warnings as validation errors"},
	},
	"lint": {
		{name: "json", desc: "Emit a versioned findings envelope"},
		{name: "github-annotations", desc: "Emit GitHub Actions file annotations"},
		{name: "check-harness", desc: "Verify referenced agent harnesses are installed and signed in"},
		{name: "check-repos", desc: "Verify target repositories are reachable"},
		{name: "source-tree", desc: "Lint a checked-in config source tree"},
		{name: "strict", desc: "Treat config warnings as validation errors"},
	},
	"up": {
		{name: "quiet", desc: "Suppress liveness heartbeats"},
		{name: "diagnostics", desc: "Capture deep per-stage diagnostics for hang debugging"},
		{name: "notify", desc: "Desktop-notify on escalated/failed runs (=all for every outcome)"},
		{name: "skip-preflight", desc: "Start despite config validation errors"},
		{name: "watch-config", desc: "Experimental: hot-reload config edits"},
		{name: "drain-timeout", takesArg: true, desc: "Force shutdown after this graceful-drain duration"},
		{name: "cleanup-spans-only-runs", desc: "Delete reported legacy spans-only run directories at startup"},
		{name: "disable-read-model-reads", desc: "Read-model rollback: force authoritative journal scans for this run"},
	},
	"fix": {
		{name: "to", takesArg: true, desc: "Target DSL version"},
		{name: "write", desc: "Apply migrations in place"},
	},
	"doctor": {
		{name: "k8s", desc: "Preflight a Kubernetes cluster"},
		{name: "repo", desc: "Compare repository forge policy with GitHub"},
		{name: "av-exclusions", desc: "List the directories Goobers writes then reads and verify antivirus exclusions (advisory)"},
		{name: "work-root", takesArg: true, desc: "Worker work root to enumerate (--av-exclusions)"},
		{name: "kubeconfig", takesArg: true, desc: "Kubeconfig path"},
		{name: "context", takesArg: true, desc: "Kubeconfig context"},
		{name: "report", takesArg: true, values: []string{"text", "json"}, desc: "Report format"},
		{name: "oidc-issuer", takesArg: true, desc: "OIDC issuer URL"},
		{name: "registry", takesArg: true, desc: "Container registry host"},
		{name: "egress", takesArg: true, desc: "Outbound host and port targets"},
		{name: "timeout", takesArg: true, desc: "Per-probe timeout"},
	},
	"netpol-render": {
		{name: "out", takesArg: true, desc: "Directory to write the rendered manifest set into"},
		{name: "check", desc: "Validate provenance markers, the coverage ratchet, and output freshness"},
		{name: "baseline", takesArg: true, desc: "Coverage baseline file"},
		{name: "write-baseline", desc: "Freeze the current per-class coverage into the baseline"},
		{name: "timeout", takesArg: true, desc: "Per-fetch timeout for provenance checks"},
		{name: "print-blob-endpoint", desc: "Print the blob endpoint (namespace, pod labels, port) as JSON and exit"},
	},
	"self-update": {
		{name: "policy", takesArg: true, values: []string{"manual", "on-release", "on-main"}, desc: "Update policy"},
		{name: "branch", takesArg: true, desc: "Branch tracked by on-main"},
		{name: "target", takesArg: true, desc: "Manual release tag"},
		{name: "health-ticks", takesArg: true, desc: "Required clean heartbeat ticks"},
		{name: "health-timeout", takesArg: true, desc: "Candidate health window"},
	},
	"service status": {
		{name: "json", desc: "Emit JSON"},
	},
	"engine-start": {
		{name: "gaggle", takesArg: true, desc: "Gaggle owning the workflow"},
		{name: "temporal-hostport", takesArg: true, desc: "Temporal frontend host and port"},
		{name: "temporal-namespace", takesArg: true, desc: "Temporal namespace"},
		{name: "task-queue", takesArg: true, desc: "Workflow task queue"},
		{name: "dedupe-key", takesArg: true, desc: "Run identity deduplication key (requires --direct)"},
		{name: "direct", desc: "Dispatch straight to Temporal instead of through the running daemon"},
		{name: "live-journal", desc: "Author the run journal live through the daemon's journal plane"},
	},
	"engine-queues": {
		{name: "temporal-hostport", takesArg: true, desc: "Temporal frontend host and port"},
		{name: "temporal-namespace", takesArg: true, desc: "Temporal namespace"},
		{name: "task-queue", takesArg: true, desc: "Workflow task queue"},
		{name: "timeout", takesArg: true, desc: "Bound on the whole describe"},
		{name: "json", desc: "Emit JSON"},
	},
	"engine-project": {
		{name: "gaggle", takesArg: true, desc: "Gaggle owning the run"},
		{name: "temporal-hostport", takesArg: true, desc: "Temporal frontend host and port"},
		{name: "temporal-namespace", takesArg: true, desc: "Temporal namespace"},
	},
	"worker": {
		{name: "instance", takesArg: true, desc: "Instance root; wires the real executors"},
		{name: "blob-store", takesArg: true, desc: "Directory backing the fleet artifact store"},
		{name: "daemon-api", takesArg: true, desc: "Daemon write API base URL for live journal emission"},
		{name: "dispatch-namespace", takesArg: true, desc: "Namespace for mode-3 stage pods; wires the dispatcher seam"},
		{name: "config-reload-interval", takesArg: true, desc: "How often to re-read the instance config tree and rebuild changed gaggle seams (0 disables)"},
		{name: "task-queue", takesArg: true, desc: "Task queue to serve (repeatable)"},
		{name: "temporal-hostport", takesArg: true, desc: "Temporal frontend host and port"},
		{name: "temporal-namespace", takesArg: true, desc: "Temporal namespace"},
		{name: "drain-timeout", takesArg: true, desc: "Graceful-drain timeout"},
		{name: "work-root", takesArg: true, desc: "Stage workspace root"},
	},
	"speech preflight": {
		{name: "json", desc: "Emit JSON"},
	},
	"speech test": {
		{name: "json", desc: "Emit JSON"},
	},
	"fleet join": {
		{name: "url", takesArg: true, desc: "Fleet service URL"},
		{name: "enrollment-token-file", takesArg: true, desc: "Private file containing the one-time enrollment grant"},
		{name: "grant-local-admin", desc: "Grant the discovered local administrator instance:read"},
		{name: "no-grant-local-admin", desc: "Explicitly enroll with an empty ACL"},
	},
	"fleet status": {
		{name: "json", desc: "Emit JSON"},
	},
	"dashboard": {
		{name: "port", takesArg: true, desc: "Dashboard port, or auto"},
		{name: "listen", takesArg: true, desc: "Bind address as host:port; non-loopback requires api.auth"},
		{name: "no-open", desc: "Print the URL without opening a browser"},
		{name: "dev-assets", takesArg: true, desc: "Serve a local portal build"},
		{name: "wait-for-daemon", desc: "Wait up to 30s for a concurrently starting daemon"},
	},
	"getting-started": {
		{name: "port", takesArg: true, desc: "Server port, or auto"},
		{name: "no-open", desc: "Print the URL without opening a browser"},
		{name: "workdir", takesArg: true, desc: "Directory holding the tutorial sample and instance"},
	},
	"run": {
		{name: "gaggle", takesArg: true, desc: "Trigger the workflow in this gaggle"},
		{name: "pr", takesArg: true, desc: "Target an exact pull request for merge-review"},
		{name: "no-wait", desc: "Return after the run is dispatched"},
	},
	"approve": {
		{name: "decision", takesArg: true, desc: "Gate decision"},
		{name: "actor", takesArg: true, desc: "Recorded actor identity"},
	},
	"override": {
		{name: "rationale", takesArg: true, desc: "Override rationale"},
		{name: "decision", takesArg: true, desc: "Gate decision"},
		{name: "actor", takesArg: true, desc: "Recorded actor identity"},
	},
	"rerun-stage": {
		{name: "addendum", takesArg: true, desc: "Instruction addendum"},
		{name: "actor", takesArg: true, desc: "Recorded actor identity"},
	},
	"workflow show": {
		{name: "dot", desc: "Emit Graphviz DOT"},
	},
	"runs list": {
		{name: "json", desc: "Emit JSON"},
		{name: "phase", takesArg: true, desc: "Filter by phase"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Filter by workflow"},
		{name: "gaggle", takesArg: true, desc: "Filter by gaggle"},
		{name: "limit", takesArg: true, desc: "Maximum runs"},
	},
	"runs du": {
		{name: "json", desc: "Emit JSON"},
	},
	"status": {
		{name: "agents", desc: "List in-flight agentic stages by role"},
		{name: "daemon", desc: "Report daemon health and identity"},
		{name: "json", desc: "Emit JSON"},
		{name: "phase", takesArg: true, desc: "Filter by phase"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Filter by workflow"},
		{name: "gaggle", takesArg: true, desc: "Filter by gaggle"},
		{name: "limit", takesArg: true, desc: "Maximum runs"},
		{name: "watch", desc: "Refresh the status board until interrupted"},
		{name: "interval", takesArg: true, desc: "Watch refresh interval"},
	},
	"stats": {
		{name: "since", takesArg: true, desc: "Only include activity from the preceding duration"},
		{name: "json", desc: "Emit JSON"},
	},
	"features": {
		{name: "json", desc: "Emit a versioned feature-discovery envelope"},
		{name: "dsl-version", takesArg: true, desc: "Scope features to one DSL version"},
		{name: "used", desc: "List only features referenced by the instance"},
	},
	"schema": {
		{name: "list", desc: "List every embedded schema kind"},
		{name: "human", desc: "Emit a human-readable rendering"},
	},
	"explain": {
		{name: "human", desc: "Emit a human-readable rendering"},
	},
	"blocked list": {
		{name: "json", desc: "Emit JSON"},
	},
	"config show": {
		{name: "json", desc: "Render the config as JSON instead of YAML"},
	},
	"config diff": {
		{name: "against", takesArg: true, desc: "Canonical config source root"},
	},
	"claims list": {
		{name: "json", desc: "Emit JSON"},
		{name: "stale", desc: "Show only expired claims"},
		{name: "gaggle", takesArg: true, desc: "Filter by gaggle"},
		{name: "provider", takesArg: true, desc: "Filter by provider"},
	},
	"claims release": {
		{name: "gaggle", takesArg: true, desc: "Gaggle owning the claim"},
		{name: "provider", takesArg: true, desc: "Provider owning the claim"},
		{name: "force", desc: "Release a claim held by a non-terminal run"},
	},
	"trace": {
		{name: "json", desc: "Emit JSON"},
		{name: "follow", desc: "Stream events until the run reaches a terminal phase"},
		{name: "summary", desc: "Show run metadata and review verdicts"},
		{name: "verdicts", desc: "Show review verdict content"},
		{name: "transcripts", desc: "Show every recorded agent-stage transcript"},
		{name: "transcript", takesArg: true, desc: "Show recorded transcript data for one stage"},
	},
	"e2e verify": {
		{name: "run", takesArg: true, valueKind: "runs", desc: "Run id to verify"},
		{name: "gaggle", takesArg: true, desc: "Require the run belong to this gaggle"},
		{name: "expected", takesArg: true, desc: "Topology expectations JSON file"},
		{name: "out", takesArg: true, desc: "Write the evidence bundle here instead of stdout"},
		{name: "print-runner-class", takesArg: true, desc: "Print the runner-class label value for a restriction set and exit"},
	},
	"e2e kill-inject": {
		{name: "run", takesArg: true, valueKind: "runs", desc: "Run id whose stage attempt to kill"},
		{name: "stage", takesArg: true, desc: "The real workflow stage name to target"},
		{name: "stage-class", takesArg: true, desc: "Which S6 stage class this stage plays: builtin, agentic, or local-ci"},
		{name: "namespace", takesArg: true, desc: "Kubernetes namespace the target pod runs in"},
		{name: "poll-timeout", takesArg: true, desc: "Bound on each polling phase"},
		{name: "out", takesArg: true, desc: "Write the injection record here instead of stdout"},
	},
	"escalations": {
		{name: "json", desc: "Emit JSON"},
	},
	"escalations show": {
		{name: "json", desc: "Emit JSON"},
		{name: "include-verdict", desc: "Include review verdict content"},
	},
	"telemetry stats": {
		{name: "json", desc: "Emit JSON"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Filter by workflow"},
		{name: "gaggle", takesArg: true, desc: "Filter by gaggle"},
		{name: "branch", takesArg: true, desc: "Filter by journal branch"},
		{name: "model", takesArg: true, desc: "Filter by model"},
		{name: "harness-version", takesArg: true, desc: "Filter by harness version"},
		{name: "group-by", takesArg: true, desc: "Group by model or harness-version"},
		{name: "since", takesArg: true, desc: "Include runs at or after this RFC3339 timestamp"},
		{name: "until", takesArg: true, desc: "Include runs at or before this RFC3339 timestamp"},
		{name: "rebuild", desc: "Rebuild telemetry from run journals before querying"},
	},
	"telemetry errors": {
		{name: "json", desc: "Emit JSON"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Filter by workflow"},
		{name: "gaggle", takesArg: true, desc: "Filter by gaggle"},
		{name: "class", takesArg: true, desc: "Filter by error class"},
		{name: "limit", takesArg: true, desc: "Maximum errors"},
		{name: "since", takesArg: true, desc: "Include errors at or after this RFC3339 timestamp"},
		{name: "until", takesArg: true, desc: "Include errors at or before this RFC3339 timestamp"},
		{name: "rebuild", desc: "Rebuild telemetry from run journals before querying"},
	},
	"telemetry prune-orphans": {
		{name: "delete", desc: "Delete eligible orphan directories (opt-in; default dry-run)"},
		{name: "min-age", takesArg: true, desc: "Minimum inactivity age (at least 24h)"},
	},
	"telemetry prune": {
		{name: "dry-run", desc: "Report eligible runs without deleting them"},
	},
	"telemetry export": {
		{name: "since", takesArg: true, desc: "Inclusive span-start lower bound"},
		{name: "until", takesArg: true, desc: "Exclusive span-start upper bound"},
	},
	"telemetry compact": {
		{name: "dry-run", desc: "Report reclaimable data without changing it"},
	},
	"journal redact": {
		{name: "run", takesArg: true, valueKind: "runs", desc: "Run id"},
		{name: "path", takesArg: true, desc: "Journal-relative blob path"},
		{name: "reason", takesArg: true, desc: "Redaction reason"},
		{name: "secret-file", takesArg: true, desc: "Read the leaked secret bytes from this file"},
	},
	"backlog-health": {
		{name: "feedback", desc: "Include backlog feedback"},
	},
	"backlog-query": {
		{name: "claim", desc: "Claim the first eligible item"},
		{name: "debug", desc: "Explain candidate eligibility and exclusions"},
		{name: "release", desc: "Release this run's claim leases early"},
		{name: "read-only", desc: "Query without mutating provider state"},
		{name: "reconcile", desc: "Reconcile claim state"},
	},
	"set-milestone": {
		{name: "item", takesArg: true, desc: "Issue item id"},
		{name: "milestone", takesArg: true, desc: "Milestone number"},
	},
	"reconcile-post-merge": {
		{name: "max", takesArg: true, desc: "Maximum pull requests to reconcile"},
		{name: "lookback", takesArg: true, desc: "Merge lookback duration"},
	},
	"reconcile-branches": {
		{name: "delete", desc: "Delete eligible branches (opt-in; default dry-run)"},
		{name: "max", takesArg: true, desc: "Maximum candidates inspected in one sweep"},
		{name: "min-age", takesArg: true, desc: "Minimum terminal run age required for deletion"},
		{name: "after", takesArg: true, desc: "Resume after this branch name in lexical order"},
	},
	"telemetry-query": {
		{name: "window", takesArg: true, desc: "Lookback window (e.g. 24h)"},
		{name: "aggregate", takesArg: true, values: []string{"all", "stage-failure-rate", "error-signature", "ci-check-failure", "gate-noise", "workflow-untriggered", "stage-unreached", "credit-assignment", "learning-episode"}, desc: "Aggregate to detect"},
		{name: "learning-action", takesArg: true, values: []string{"instruction-or-skill", "workflow-or-gate", "targeted-test-mapping", "code-issue"}, desc: "Governed learning action to include"},
		{name: "threshold", takesArg: true, desc: "Threshold override k=v"},
		{name: "format", takesArg: true, values: []string{"candidate-findings", "effective-version-efficacy", "tutor-live-verification"}, desc: "Artifact format"},
		{name: "gaggle", takesArg: true, desc: "Gaggle to query"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Workflow keying the query"},
	},
	"docs-churn": {
		{name: "repo", takesArg: true, desc: "Git repository/worktree to scan"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Workflow keying the watermark"},
		{name: "gaggle", takesArg: true, desc: "Gaggle keying the watermark"},
		{name: "since", takesArg: true, desc: "First-run window and minimum buffer floor"},
		{name: "buffer-multiplier", takesArg: true, desc: "Buffer multiplier over observed churn"},
		{name: "format", takesArg: true, values: []string{"churn-digest"}, desc: "Artifact format"},
	},
	"ios-simulator-test": {
		{name: "project", takesArg: true, desc: "Xcode project path"},
		{name: "workspace", takesArg: true, desc: "Xcode workspace path"},
		{name: "scheme", takesArg: true, desc: "Xcode scheme"},
		{name: "device", takesArg: true, desc: "Simulator device"},
		{name: "runtime", takesArg: true, desc: "Simulator runtime"},
		{name: "only-testing", takesArg: true, desc: "Test target filter"},
		{name: "result-bundle", takesArg: true, desc: "Result bundle path"},
	},
	"gather-sibling-context": {
		{name: "no-cache", desc: "Bypass the sibling-context cache"},
		{name: "no-verdict-cache", desc: "Skip the verdict-cache lookup, forcing a fresh review"},
	},
	"apply-verdict": {
		{name: "gate", takesArg: true, desc: "Gate name whose verdict to apply"},
	},
	"elect-lander": {
		{name: "gate", takesArg: true, desc: "Gate name whose verdict to read"},
	},
	"pr-claim": {
		{name: "release", desc: "Release the remediation claim"},
	},
	"remediation-checkpoint": {
		{name: "budget", takesArg: true, desc: "Per-PR repass-cycle budget before escalating"},
		{name: "escalate", takesArg: true, desc: "Escalate unconditionally with this reason"},
		{name: "escalation-outcome", takesArg: true, desc: "Recorded escalation outcome"},
	},
	"respond-to-findings": {
		{name: "check", desc: "Validate without publishing responses"},
	},
	"file-issues": {
		{name: "check", desc: "Validate and scan without creating issues"},
	},
	"mcp-io": {
		{name: "config", takesArg: true, desc: "MCP server configuration path"},
	},
}

// buildCompletionModel walks the cliCommand registry and produces the
// completion model, annotating each command with its flags and positional arg
// kind. Command and subcommand names come entirely from the registry.
func buildCompletionModel() completionModel {
	var m completionModel
	for _, c := range cliCommands {
		if len(c.names) == 0 {
			continue
		}
		if isHiddenCompletionCommand(c.names[0]) {
			continue
		}
		word, aliases := splitCompletionNames(c.names)
		m.globalFlags = append(m.globalFlags, aliases...)
		if word == "" {
			continue
		}
		m.commands = append(m.commands, buildCompletionCommand(c, word, word))
	}
	return m
}

func buildCompletionCommand(c cliCommand, name, id string) completionCommand {
	node := completionCommand{
		name:      name,
		id:        id,
		desc:      c.short,
		tier:      c.tier,
		flags:     completionFlagSpecs[id],
		argKind:   completionPositionalArgKinds[id],
		argValues: completionPositionalArgValues[id],
	}
	for _, sub := range c.subcommands {
		if len(sub.names) == 0 || isHiddenCompletionCommand(sub.names[0]) {
			continue
		}
		subName := sub.names[0]
		node.subs = append(node.subs, buildCompletionCommand(sub, subName, id+" "+subName))
	}
	return node
}

// isHiddenCompletionCommand reports whether a registry name is an internal
// entrypoint that must never appear in completion: the __-prefixed helpers
// (__complete) and the detached-run worker. The same exclusions the help
// golden walk applies, minus the single-dash version/help aliases, which
// completion surfaces via splitCompletionNames instead.
func isHiddenCompletionCommand(name string) bool {
	return strings.HasPrefix(name, "__") || name == detachedRunWorkerCommand
}

// splitCompletionNames separates a registry entry's names into the word-form
// command (the first non-dash name) and its dash-prefixed aliases. For a normal
// command this is just (name, nil); for the version/help alias entries it is
// e.g. ("version", ["--version"]) and ("help", ["-h", "--help"]).
func splitCompletionNames(names []string) (word string, aliases []string) {
	for _, n := range names {
		if strings.HasPrefix(n, "-") {
			aliases = append(aliases, n)
			continue
		}
		if word == "" {
			word = n
		}
	}
	return word, aliases
}
