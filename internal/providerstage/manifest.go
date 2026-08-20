// Package providerstage describes the capabilities used by built-in
// provider-chain commands.
package providerstage

import (
	"slices"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
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
}

// Command describes one built-in provider-chain command.
type Command struct {
	ResultFile   string
	Capabilities []CapabilityUse

	mutatesClaimLedger bool
	claimMutationFlags []string
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

// Commands returns every command declared by the provider-stage manifest.
func Commands() []string {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Lookup returns the manifest entry for command.
func Lookup(command string) (Command, bool) {
	entry, ok := commands[command]
	if !ok {
		return Command{}, false
	}
	entry.Capabilities = append([]CapabilityUse(nil), entry.Capabilities...)
	return entry, true
}

// RequiredCapabilities returns the capabilities command must declare for args.
func RequiredCapabilities(command string, args []string) []CapabilityUse {
	entry, ok := Lookup(command)
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

// ResultFile returns the default result file for a guarded provider stage.
func ResultFile(command string) (string, bool) {
	entry, ok := Lookup(command)
	if !ok || entry.ResultFile == "" {
		return "", false
	}
	return entry.ResultFile, true
}

// MutatesClaimLedger reports whether a built-in invocation can write claims.json.
func MutatesClaimLedger(command string, args []string) bool {
	entry, ok := Lookup(command)
	if !ok {
		return false
	}
	return entry.mutatesClaimLedger || anyFlagEnabled(args, entry.claimMutationFlags)
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
