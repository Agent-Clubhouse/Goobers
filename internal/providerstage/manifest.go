// Package providerstage describes the capabilities used by built-in
// provider-chain commands.
//
// # DSL-version linkage
//
// The manifest is the admission contract for every already-shipped config:
// each interpreter package compiles workflows against it, so an unversioned
// requirement edit used to land on every DSL version at once, with no
// deprecation window. That is the ed11ae81 incident class (TBH-1/#2386):
// narrowing backlog-dedupe's requirement broke every existing config in one
// commit, on every DSL version simultaneously. To keep that class impossible
// the table carries per-entry version linkage (dsl-3.0.md D7/§8, issue
// #3504): a Command and each of its CapabilityUse rows may declare the DSL
// version window in which they exist — sinceDSL inclusive, untilDSL
// exclusive, both empty meaning baseline (every version). Interpreters never
// read the table directly; each resolves the view for its own version via
// ForVersion, so a requirement change gated to a later DSL version is
// invisible to earlier interpreters: the old requirement keeps admitting
// old-version workflows while the new one applies only from the version that
// introduced it. A requirement swap at a version boundary is written as two
// rows — the old use closed with untilDSL, the new use opened with sinceDSL.
//
// Version windows fail closed: a bound that does not parse as a
// "<major>.<minor>" DSL version hides the gated element, and a view resolved
// for an unparseable version sees only baseline (unbounded) elements.
//
// Commands, Lookup, and ResultFile deliberately ignore the version linkage:
// they are the runtime/tooling surface (result-file defaults, CLI dispatch
// conformance, the builtincmd inventory parity), which must cover the union
// of every version's entries. Admission is the only version-gated consumer.
package providerstage

import (
	"slices"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// CapabilityUse describes one capability a built-in command can use.
type CapabilityUse struct {
	Capability  capability.Capability
	Consequence string

	optional    bool
	flag        string
	flagValue   string
	anyFlags    []string
	unlessFlags []string

	// sinceDSL/untilDSL bound the DSL versions in which this requirement
	// exists ([sinceDSL, untilDSL); empty bounds are open). A requirement
	// change at a version boundary is two rows: the old use closed with
	// untilDSL, its replacement opened with sinceDSL — earlier interpreters
	// keep resolving the old row, so the change cannot re-break shipped
	// configs (the ed11ae81 class; see the package comment).
	sinceDSL string
	untilDSL string
}

// Command describes one built-in provider-chain command.
type Command struct {
	ResultFile   string
	Capabilities []CapabilityUse

	mutatesClaimLedger bool
	claimMutationFlags []string

	// sinceDSL/untilDSL bound the DSL versions in which the command exists,
	// with the same window semantics as CapabilityUse. Entry metadata
	// (ResultFile, the claim-ledger bits) is not versionable within one
	// window: changing it at a version boundary needs the table to grow
	// non-overlapping windowed entries per command name, deliberately
	// deferred until a real change needs it.
	sinceDSL string
	untilDSL string
}

func required(cap capability.Capability, consequence string) CapabilityUse {
	return CapabilityUse{Capability: cap, Consequence: consequence}
}

func optional(cap capability.Capability, consequence string) CapabilityUse {
	return CapabilityUse{Capability: cap, Consequence: consequence, optional: true}
}

func requiredWhenFlagEquals(cap capability.Capability, flag, value, consequence string) CapabilityUse {
	return CapabilityUse{
		Capability:  cap,
		Consequence: consequence,
		flag:        flag,
		flagValue:   value,
	}
}

func requiredWhenAnyFlag(cap capability.Capability, flags []string, consequence string) CapabilityUse {
	return CapabilityUse{Capability: cap, Consequence: consequence, anyFlags: flags}
}

func requiredUnlessAnyFlag(cap capability.Capability, flags []string, consequence string) CapabilityUse {
	return CapabilityUse{Capability: cap, Consequence: consequence, unlessFlags: flags}
}

var commands = map[string]Command{
	"apply-verdict": {
		ResultFile: "verdict-result.json",
		Capabilities: []CapabilityUse{
			required(capability.ProviderPRWrite, "the capability-scoped credential is not injected, so pull-request routing fails at runtime"),
			required(capability.GitHubPRReview, "the capability-scoped credential is not injected, so native review publication fails at runtime"),
		},
	},
	"backlog-dedupe": {
		ResultFile: "dedupe-candidates.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubIssuesRead, "the read-only capability-scoped credential is not injected, so backlog duplicate discovery fails at runtime"),
		},
	},
	"backlog-assignment": {
		ResultFile: "backlog-assignment.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so backlog assignment fails at runtime"),
		},
	},
	"backlog-health": {
		ResultFile: "backlog-health.json",
		Capabilities: []CapabilityUse{
			requiredWhenAnyFlag(capability.GitHubIssuesWrite, []string{"feedback"}, "the write capability-scoped credential is not injected, so implementation feedback fails at runtime"),
			requiredUnlessAnyFlag(capability.GitHubIssuesRead, []string{"feedback"}, "the read-only capability-scoped credential is not injected, so backlog health sampling fails at runtime"),
		},
	},
	"backlog-query": {
		ResultFile: "claimed-item.json",
		Capabilities: []CapabilityUse{
			requiredWhenAnyFlag(capability.GitHubIssuesRead, []string{"read-only"}, "the read-only capability-scoped credential is not injected, so read-only backlog queries fail at runtime"),
			requiredUnlessAnyFlag(capability.GitHubIssuesWrite, []string{"read-only"}, "the write capability-scoped credential is not injected, so backlog query and mutation operations fail at runtime"),
			optional(capability.GitHubPRWrite, "open pull-request filtering is disabled when its capability-scoped credential is not injected"),
		},
		claimMutationFlags: []string{"claim", "reconcile", "release"},
	},
	"publish-batch": {
		ResultFile: "published-batch.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so decomposition batch publication fails at runtime"),
		},
	},
	"select-source": {
		ResultFile:         "selection.json",
		mutatesClaimLedger: true,
		Capabilities: []CapabilityUse{
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so parent-issue lookup, comment listing, and claiming fail at runtime"),
		},
	},
	"validate-plan": {
		ResultFile: "plan-validation.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubIssuesRead, "the capability-scoped credential is not injected, so the live-parent conflict check fails at runtime"),
		},
	},
	"elect-lander": {
		ResultFile: "election.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so landing-candidate inspection fails at runtime"),
		},
	},
	"gather-ci-failures": {
		ResultFile: "remediation-brief.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so CI failure collection fails at runtime"),
		},
	},
	"gather-implement-context": {
		ResultFile: "implementation-context.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so implementation context collection fails at runtime"),
		},
	},
	"check-issue-staleness": {
		ResultFile: "issue-staleness-result.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so polling and labeling the pull request fails at runtime"),
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so the pinned originating issue lookup fails at runtime"),
		},
	},
	"gather-issue-context": {
		ResultFile: "remediation-brief.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so pull-request context lookup fails at runtime"),
			required(capability.GitHubIssuesRead, "the read-only capability-scoped credential is not injected, so originating issue lookup fails at runtime"),
		},
	},
	"gather-pr-context": {
		ResultFile:         "remediation-brief.json",
		mutatesClaimLedger: true,
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so remediation pull-request selection fails at runtime"),
			required(capability.RepoPush, "the capability-scoped credential is not injected, so remediation branch preparation fails at runtime"),
		},
	},
	"gather-review-threads": {
		ResultFile: "remediation-brief.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so review-thread collection fails at runtime"),
		},
	},
	"gather-sibling-context": {
		ResultFile: "sibling-context.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so sibling pull-request collection fails at runtime"),
		},
	},
	"issue-close-out": {
		ResultFile:         "issue-close-out-result.json",
		mutatesClaimLedger: true,
		Capabilities: []CapabilityUse{
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so issue close-out fails at runtime"),
		},
	},
	"merge-pr": {
		ResultFile: "merge-result.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRMerge, "the capability-scoped credential is not injected, so pull-request merge fails at runtime"),
			required(capability.GitHubBranchDelete, "the capability-scoped credential is not injected, so merged-branch cleanup fails at runtime"),
			// Azure DevOps land: the ADO branch resolves ado:pr:complete (the ADO
			// counterpart to github:pr:merge) via providerToken to preserve the
			// decider≠executor grant isolation. Marked optional — it is
			// provider-conditional (used only when repo.Provider is ADO), so it
			// must NOT be auto-derived onto GitHub merge-pr tasks; the ADO
			// merge-review workflow declares it explicitly on this stage instead.
			optional(capability.ADOPRComplete, "the capability-scoped credential is not injected, so Azure DevOps pull-request completion fails at runtime"),
		},
	},
	"merge-queue-poll": {
		ResultFile: "queue-result.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRMerge, "the capability-scoped credential is not injected, so merge-queue polling fails at runtime"),
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so eviction remediation fails at runtime"),
			required(capability.GitHubBranchDelete, "the capability-scoped credential is not injected, so queue-merged branch cleanup fails at runtime"),
			optional(capability.ADOPRComplete, "the capability-scoped credential is not injected, so Azure DevOps queue completion fails at runtime"),
		},
	},
	"open-pr": {
		ResultFile: "pr-result.json",
		Capabilities: []CapabilityUse{
			required(capability.ProviderPRWrite, "the configured provider's capability-scoped credential is not available, so pull-request creation fails at runtime"),
		},
	},
	"post-merge": {
		ResultFile: "post-merge-result.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so merged pull-request inspection fails at runtime"),
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so post-merge issue updates fail at runtime"),
		},
	},
	"pr-select": {
		ResultFile:         "selected-pr.json",
		mutatesClaimLedger: true,
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so pull-request selection fails at runtime"),
		},
	},
	"pr-claim": {
		ResultFile:         "pr-remediation-lifecycle.json",
		mutatesClaimLedger: true,
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so remediation pull-request state checks fail at runtime"),
		},
	},
	"report-pr-status": {
		ResultFile: "status-result.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so publishing the pull-request status fails at runtime (the GitHub PR-write grant also authorizes the Gitea commit-status path)"),
		},
	},
	"push-remediated": {
		ResultFile: "push-remediated-result.json",
		Capabilities: []CapabilityUse{
			required(capability.RepoPush, "the capability-scoped credential is not injected, so remediated branch publication fails at runtime"),
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so remediated pull-request refresh fails at runtime"),
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so remediation label cleanup fails at runtime"),
		},
	},
	"rebase-pr": {
		ResultFile: "rebase-result.json",
		Capabilities: []CapabilityUse{
			required(capability.RepoPush, "the capability-scoped credential is not injected, so rebased branch publication fails at runtime"),
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so rebase outcome routing fails at runtime"),
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so trusted sibling-overlap handoff inspection fails at runtime"),
		},
	},
	"reconcile-post-merge": {
		ResultFile: "reconcile-post-merge-result.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so late merge inspection fails at runtime"),
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so late merge issue reconciliation fails at runtime"),
			required(capability.GitHubBranchDelete, "the capability-scoped credential is not injected, so late merge branch cleanup fails at runtime"),
		},
	},
	"remediation-checkpoint": {
		ResultFile: "checkpoint-result.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so remediation checkpoint inspection fails at runtime"),
			required(capability.RepoPush, "the capability-scoped credential is not injected, so remediation checkpoint branch inspection fails at runtime"),
		},
	},
	"respond-to-findings": {
		ResultFile: "remediation-response.json",
		Capabilities: []CapabilityUse{
			requiredUnlessAnyFlag(capability.GitHubIssuesWrite, []string{"check"}, "the capability-scoped credential is not injected, so finding responses cannot be published at runtime"),
		},
	},
	"resolve-review-threads": {
		ResultFile: "review-thread-resolution.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so review-thread replies and resolutions fail at runtime"),
		},
	},
	"set-milestone": {
		ResultFile: "milestone-result.json",
		Capabilities: []CapabilityUse{
			required(capability.GitHubMilestonesWrite, "the capability-scoped credential is not injected, so milestone assignment fails at runtime"),
		},
	},
	"update-behind-pr": {
		ResultFile:         "update-behind-result.json",
		mutatesClaimLedger: true,
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so behind-base pull-request update fails at runtime"),
			required(capability.GitHubIssuesWrite, "the capability-scoped credential is not injected, so behind-base remediation routing fails at runtime"),
		},
	},
	"push-branch": {
		Capabilities: []CapabilityUse{
			required(capability.RepoPush, "the capability-scoped credential is not injected, so branch publication fails at runtime"),
		},
	},
	"reconcile-branches": {
		Capabilities: []CapabilityUse{
			required(capability.GitHubBranchDelete, "the capability-scoped credential is not injected, so stale branch reconciliation fails at runtime"),
		},
	},
	"record-merge-refusal": {
		Capabilities: []CapabilityUse{
			required(capability.GitHubPRWrite, "the capability-scoped credential is not injected, so merge-refusal recording fails at runtime"),
		},
	},
	"telemetry-query": {
		Capabilities: []CapabilityUse{
			required(capability.TelemetryRead, "telemetry access is not admitted, so the connector would read telemetry without declared authority at runtime"),
			requiredWhenFlagEquals(capability.GitHubPRWrite, "--format", "tutor-live-verification", "the capability-scoped credential is not injected, so Tutor holdout merge-state refresh fails at runtime"),
		},
	},
}

// Manifest is the manifest resolved at one DSL version: the view an
// interpreter compiles and admits workflows against. Two interpreters
// resolving different versions may disagree about a command's requirements —
// that disagreement is the deprecation window the unversioned table lacked.
type Manifest struct {
	dslVersion string
}

// ForVersion resolves the version-filtered manifest view for dslVersion.
// Each interpreter resolves exactly one view, at its own DSLVersion constant.
func ForVersion(dslVersion string) Manifest {
	return Manifest{dslVersion: dslVersion}
}

// RequiredCapabilities returns the capabilities command must declare for args
// as seen at this view's DSL version.
func (m Manifest) RequiredCapabilities(command string, args []string) []CapabilityUse {
	return requiredCapabilitiesIn(commands, m.dslVersion, command, args)
}

// MutatesClaimLedger reports whether a built-in invocation can write
// claims.json as seen at this view's DSL version.
func (m Manifest) MutatesClaimLedger(command string, args []string) bool {
	return mutatesClaimLedgerIn(commands, m.dslVersion, command, args)
}

func requiredCapabilitiesIn(table map[string]Command, dslVersion, command string, args []string) []CapabilityUse {
	entry, ok := lookupIn(table, dslVersion, command)
	if !ok {
		return nil
	}
	var required []CapabilityUse
	for _, use := range entry.Capabilities {
		if use.required(args) {
			required = append(required, use)
		}
	}
	return required
}

func mutatesClaimLedgerIn(table map[string]Command, dslVersion, command string, args []string) bool {
	entry, ok := lookupIn(table, dslVersion, command)
	if !ok {
		return false
	}
	return entry.mutatesClaimLedger || anyFlagEnabled(args, entry.claimMutationFlags)
}

// lookupIn resolves command's entry in table as seen at dslVersion: an entry
// outside its version window is absent, and capability uses outside theirs
// are filtered out. The resolved entry is version-less — window metadata is
// stripped, because a resolved view answers "what exists at this version",
// never "when" — which also makes two tables' views directly comparable. The
// table parameter exists so tests can prove the filtering against synthetic
// version-gated tables.
func lookupIn(table map[string]Command, dslVersion, command string) (Command, bool) {
	entry, ok := table[command]
	if !ok || !activeAt(dslVersion, entry.sinceDSL, entry.untilDSL) {
		return Command{}, false
	}
	uses := make([]CapabilityUse, 0, len(entry.Capabilities))
	for _, use := range entry.Capabilities {
		if !activeAt(dslVersion, use.sinceDSL, use.untilDSL) {
			continue
		}
		use.sinceDSL, use.untilDSL = "", ""
		uses = append(uses, use)
	}
	entry.Capabilities = uses
	entry.sinceDSL, entry.untilDSL = "", ""
	return entry, true
}

// activeAt reports whether an element with version window [since, until)
// exists at dslVersion; empty bounds are open. Malformed versions fail
// closed: a bound that does not parse hides the gated element, and an
// unparseable view version sees only baseline (unbounded) elements.
func activeAt(dslVersion, since, until string) bool {
	if since != "" {
		order, ok := supportmatrix.CompareDSLVersions(dslVersion, since)
		if !ok || order < 0 {
			return false
		}
	}
	if until != "" {
		order, ok := supportmatrix.CompareDSLVersions(dslVersion, until)
		if !ok || order >= 0 {
			return false
		}
	}
	return true
}

// Commands returns every command declared by the provider-stage manifest at
// any DSL version. Like Lookup and ResultFile it ignores the version linkage:
// the runtime/tooling surface covers the union of every version's entries.
func Commands() []string {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Lookup returns the manifest entry for command, ignoring version linkage
// (see Commands). Admission resolves entries through ForVersion instead.
func Lookup(command string) (Command, bool) {
	entry, ok := commands[command]
	if !ok {
		return Command{}, false
	}
	entry.Capabilities = append([]CapabilityUse(nil), entry.Capabilities...)
	return entry, true
}

// ResultFile returns the default result file for a guarded provider stage,
// ignoring version linkage (see Commands): the executor consults it for
// whatever command an admitted workflow of any version actually runs.
func ResultFile(command string) (string, bool) {
	entry, ok := Lookup(command)
	if !ok || entry.ResultFile == "" {
		return "", false
	}
	return entry.ResultFile, true
}

func (u CapabilityUse) required(args []string) bool {
	if u.optional {
		return false
	}
	if len(u.anyFlags) > 0 {
		return anyFlagEnabled(args, u.anyFlags)
	}
	if len(u.unlessFlags) > 0 {
		return !anyFlagEnabled(args, u.unlessFlags)
	}
	if u.flag == "" {
		return true
	}
	flagName := strings.TrimLeft(u.flag, "-")
	for i, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		if name != "-"+flagName && name != "--"+flagName {
			continue
		}
		if u.flagValue == "" {
			return true
		}
		if hasValue && value == u.flagValue {
			return true
		}
		if !hasValue && i+1 < len(args) && args[i+1] == u.flagValue {
			return true
		}
	}
	return false
}

func anyFlagEnabled(args, flags []string) bool {
	enabled := make(map[string]bool, len(flags))
	for _, arg := range args {
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			break
		}
		name := strings.TrimPrefix(arg, "-")
		name = strings.TrimPrefix(name, "-")
		name, value, hasValue := strings.Cut(name, "=")
		for _, flag := range flags {
			if name != flag {
				continue
			}
			parsed := true
			if hasValue {
				var err error
				parsed, err = strconv.ParseBool(value)
				if err != nil {
					continue
				}
			}
			enabled[flag] = parsed
		}
	}
	for _, value := range enabled {
		if value {
			return true
		}
	}
	return false
}
