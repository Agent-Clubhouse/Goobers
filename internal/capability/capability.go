// Package capability is the canonical registry of capability strings a Goober
// stage or a dedicated runner component may hold (ARCHITECTURE.md §5,
// SEC-042/SEC-044). It is the single source of truth the DSL compiler
// validates declarations against (internal/workflow), and that other
// capability-scoped seams — the credential injector (internal/credentials),
// the telemetry query gate (internal/telemetry/query) — name their own
// constants from, so a typo'd or invented capability string is caught at
// compile/validate time rather than silently accepted until admission, or
// never caught at all (issue #74: `github:pr:write` vs. an issue-text typo of
// `github:prs:write` slipping through unnoticed because nothing checked either
// spelling against a ground truth).
//
// This package has no dependencies beyond the stdlib, so every layer that
// needs to name a capability — including packages that deliberately avoid
// importing api/v1alpha1 or internal/journal to stay decoupled from the wire
// contract or the runner (internal/telemetry/query's own doc comment) — can
// depend on it without pulling in anything heavier.
package capability

// Capability identifies a scoped grant a stage or dedicated runner component
// may hold. The dotted "resource:verb" shape mirrors provider/credential
// capability strings already in use (ARCHITECTURE.md §5).
type Capability string

// The canonical V0 capability set. Add new entries here first; the compiler
// rejects anything not listed (WithGoobers-gated admission,
// internal/workflow/compile.go), so a definition can never drift ahead of this
// registry. Runner-only entries must also be excluded by StageDeclarable.
const (
	// RepoRead grants a read-only checkout of the target repository's per-stage
	// worktree — no push. Added for the work-nomination workflow (issue #26):
	// its nominator goober analyzes the codebase for gaps but writes issues
	// only, never code.
	RepoRead Capability = "repo:read"
	// RepoPush grants `git push` to the target repository's per-stage worktree.
	RepoPush Capability = "repo:push"
	// ConfigRepoRead grants the daemon's workflow-config source read access to
	// its repository. It is runner-only: workflow stages and goobers cannot
	// declare it, and its credential never comes from a target repository.
	ConfigRepoRead Capability = "configrepo:read"
	// GitHubIssuesWrite grants GitHub issue query/create/ordinary-label/close/
	// comment operations (the backlog-curation and work-nomination workflows'
	// surface). It does not grant the trust decision represented by
	// goobers:approved.
	GitHubIssuesWrite Capability = "github:issues:write"
	// GitHubMilestonesWrite grants assignment of an existing GitHub milestone
	// to an issue. It is separate from ordinary issue writes so roadmap
	// mutation must be explicitly declared by the workflow stage.
	GitHubMilestonesWrite Capability = "github:milestones:write"
	// GitHubIssuesApprove grants the narrow authority to apply
	// goobers:approved to a nominated issue. It is separate from general issue
	// writes so a workflow must explicitly opt into approving its own output.
	GitHubIssuesApprove Capability = "github:issues:approve"
	// GitHubPRWrite grants GitHub PR open/poll/close (the implementation
	// workflow's open-pr and ci-poll stages).
	GitHubPRWrite Capability = "github:pr:write"
	// GitHubPRReview grants submission of provider-native approve/request-
	// changes reviews. It is separate from GitHubPRWrite so review authority
	// can use a distinct identity from the PR author.
	GitHubPRReview Capability = "github:pr:review"
	// GitHubBranchDelete grants deletion of a remote GitHub branch ref —
	// after a pull request merges and no open pull request uses it as a
	// base (#605), or when a terminal run's pushed branch never became a
	// PR at all (#607/#575).
	GitHubBranchDelete Capability = "github:branch:delete"
	// GitHubPRMerge grants GitHub PR merge (issue #360) — deliberately
	// separate from GitHubPRWrite so it can be granted narrowly to
	// `merge-review` alone: `implementation` and `pr-remediation` push
	// code and open/update PRs, but must never be able to merge one
	// (docs/design/v0/pr-lifecycle-loop.md §7, "capability isolation, the
	// clinching reason" a decider and an executor stay separate workflows).
	GitHubPRMerge Capability = "github:pr:merge"
	// TelemetryRead grants read access to the local telemetry rollup
	// (stats/errors/spans) — internal/telemetry/query's gate.
	TelemetryRead Capability = "telemetry:read"
	// JournalRead grants an agentic stage read-only, digest-verified access
	// to ANOTHER run's journal (issue #103/T3: the Tutor's analyst stage
	// resolving evidence for runs flagged by a cross-run telemetry query).
	// Distinct from the same-run context-pointer resolution #121 already
	// wired unconditionally — that needs no capability because a stage's own
	// upstream artifacts are never a trust boundary; a DIFFERENT run's
	// journal is, so crossing it is capability-gated and fails closed when
	// undeclared (internal/harness's materializeContext).
	JournalRead Capability = "journal:read"
	// AgentModel grants the agentic harness authentication for its model
	// backend — harness-neutral, exactly as RepoPush is a capability while
	// GH_TOKEN is the copilot adapter's chosen env var for it. The copilot
	// adapter maps it to COPILOT_GITHUB_TOKEN (issue #288), which the Copilot
	// CLI prefers over GH_TOKEN for model auth, so an agentic stage can carry a
	// personal "Copilot Requests" PAT for the model AND an org-repo token for
	// the github tool at once (issue #287, multi-token credentials). It is
	// deliberately NOT in the runner's auto-granted credentialedCapabilities set
	// — it must be sourced explicitly via instance.yaml's credentials: block, so
	// it never silently defaults to the repo token.
	AgentModel Capability = "agent:model"
)

// All returns every canonical capability, in declaration order.
func All() []Capability {
	return []Capability{RepoRead, RepoPush, ConfigRepoRead, GitHubIssuesWrite, GitHubMilestonesWrite, GitHubIssuesApprove, GitHubPRWrite, GitHubPRReview, GitHubBranchDelete, GitHubPRMerge, TelemetryRead, JournalRead, AgentModel}
}

// Known reports whether s is a canonical capability string.
func Known(s string) bool {
	for _, c := range All() {
		if string(c) == s {
			return true
		}
	}
	return false
}

// StageDeclarable reports whether s is a canonical capability that workflow
// tasks and goobers may declare. Runner-owned capabilities stay in the same
// canonical registry but are admitted only by their dedicated runner consumer.
func StageDeclarable(s string) bool {
	return Known(s) && s != string(ConfigRepoRead)
}

// Suggest returns the closest canonical capability for a likely typo.
func Suggest(s string) (Capability, bool) {
	if Known(s) {
		return "", false
	}
	bestDistance := -1
	var best Capability
	for _, candidate := range All() {
		distance := editDistance(s, string(candidate))
		if bestDistance == -1 || distance < bestDistance {
			bestDistance = distance
			best = candidate
		}
	}
	if bestDistance > 2 {
		return "", false
	}
	return best, true
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(
				current[j-1]+1,
				previous[j]+1,
				previous[j-1]+cost,
			)
		}
		previous = current
	}
	return previous[len(b)]
}
