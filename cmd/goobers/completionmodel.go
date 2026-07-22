package main

import "strings"

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
	name    string               // canonical leaf name (registry names[0])
	id      string               // full space-joined invocation path
	desc    string               // registry short help (renders as the zsh description)
	subs    []completionCommand  // nested subcommands, from the registry
	flags   []completionFlagSpec // annotated flags (completionFlagSpecs[id])
	argKind string               // dynamic positional arg kind (workflows|runs|escalations)
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
	"workflow show":    "workflows",
}

// completionFlagSpecs maps a command id to its completable flags. The set and
// spelling of each flag is sourced from that command's real flag.FlagSet (the
// authoritative definition); -h/--help is universal and added by the renderer,
// so it is not repeated here.
var completionFlagSpecs = map[string][]completionFlagSpec{
	"init": {
		{name: "demo", desc: "Seed a credential-free runnable demo workflow"},
	},
	"scaffold goober": {
		{name: "force", desc: "Replace generated files that already exist"},
	},
	"scaffold workflow": {
		{name: "force", desc: "Replace generated files that already exist"},
	},
	"validate": {
		{name: "check-harness", desc: "Verify referenced agent harnesses are installed and signed in"},
		{name: "check-repos", desc: "Verify target repositories are reachable"},
		{name: "source-tree", desc: "Validate a checked-in config source tree"},
	},
	"up": {
		{name: "quiet", desc: "Suppress liveness heartbeats"},
		{name: "diagnostics", desc: "Capture deep per-stage diagnostics for hang debugging"},
		{name: "notify", desc: "Desktop-notify on escalated/failed runs (=all for every outcome)"},
		{name: "watch-config", desc: "Experimental: hot-reload config edits"},
		{name: "cleanup-spans-only-runs", desc: "Delete reported legacy spans-only run directories at startup"},
	},
	"dashboard": {
		{name: "port", takesArg: true, desc: "Dashboard port, or auto"},
		{name: "no-open", desc: "Print the URL without opening a browser"},
		{name: "dev-assets", takesArg: true, desc: "Serve a local portal build"},
	},
	"run": {
		{name: "no-wait", desc: "Return after the run is dispatched"},
	},
	"workflow show": {
		{name: "dot", desc: "Emit Graphviz DOT"},
	},
	"runs list": {
		{name: "json", desc: "Emit JSON"},
		{name: "phase", takesArg: true, desc: "Filter by phase"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Filter by workflow"},
		{name: "limit", takesArg: true, desc: "Maximum runs"},
	},
	"runs du": {
		{name: "json", desc: "Emit JSON"},
	},
	"status": {
		{name: "daemon", desc: "Report daemon health and identity"},
		{name: "json", desc: "Emit JSON"},
		{name: "phase", takesArg: true, desc: "Filter by phase"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Filter by workflow"},
		{name: "limit", takesArg: true, desc: "Maximum runs"},
		{name: "watch", desc: "Refresh the status board until interrupted"},
		{name: "interval", takesArg: true, desc: "Watch refresh interval"},
	},
	"stats": {
		{name: "since", takesArg: true, desc: "Only include activity from the preceding duration"},
		{name: "json", desc: "Emit JSON"},
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
		{name: "transcripts", desc: "Show every recorded agent-stage transcript"},
		{name: "transcript", takesArg: true, desc: "Show recorded transcript data for one stage"},
	},
	"escalations": {
		{name: "json", desc: "Emit JSON"},
	},
	"escalations show": {
		{name: "json", desc: "Emit JSON"},
	},
	"telemetry stats": {
		{name: "json", desc: "Emit JSON"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Filter by workflow"},
		{name: "gaggle", takesArg: true, desc: "Filter by gaggle"},
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
	"journal redact": {
		{name: "run", takesArg: true, valueKind: "runs", desc: "Run id"},
		{name: "path", takesArg: true, desc: "Journal-relative blob path"},
		{name: "reason", takesArg: true, desc: "Redaction reason"},
		{name: "secret-file", takesArg: true, desc: "Read the leaked secret bytes from this file"},
	},
	"backlog-query": {
		{name: "claim", desc: "Claim the first eligible item"},
		{name: "release", desc: "Release this run's claim leases early"},
	},
	"reconcile-branches": {
		{name: "delete", desc: "Delete eligible branches (opt-in; default dry-run)"},
		{name: "max", takesArg: true, desc: "Maximum candidates inspected in one sweep"},
		{name: "min-age", takesArg: true, desc: "Minimum terminal run age required for deletion"},
		{name: "after", takesArg: true, desc: "Resume after this branch name in lexical order"},
	},
	"telemetry-query": {
		{name: "window", takesArg: true, desc: "Lookback window (e.g. 24h)"},
		{name: "aggregate", takesArg: true, values: []string{"all", "stage-failure-rate", "error-signature", "gate-noise"}, desc: "Aggregate to detect"},
		{name: "threshold", takesArg: true, desc: "Threshold override k=v"},
		{name: "format", takesArg: true, values: []string{"candidate-findings"}, desc: "Artifact format"},
	},
	"docs-churn": {
		{name: "repo", takesArg: true, desc: "Git repository/worktree to scan"},
		{name: "workflow", takesArg: true, valueKind: "workflows", desc: "Workflow keying the watermark"},
		{name: "gaggle", takesArg: true, desc: "Gaggle keying the watermark"},
		{name: "since", takesArg: true, desc: "First-run window and minimum buffer floor"},
		{name: "buffer-multiplier", takesArg: true, desc: "Buffer multiplier over observed churn"},
		{name: "format", takesArg: true, values: []string{"churn-digest"}, desc: "Artifact format"},
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
	"remediation-checkpoint": {
		{name: "budget", takesArg: true, desc: "Per-PR repass-cycle budget before escalating"},
		{name: "escalate", takesArg: true, desc: "Escalate unconditionally with this reason"},
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
		name:    name,
		id:      id,
		desc:    c.short,
		flags:   completionFlagSpecs[id],
		argKind: completionPositionalArgKinds[id],
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
