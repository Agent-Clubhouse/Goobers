// Package capability is the canonical registry of capability strings a Goober
// may grant and a Task/Gate may declare use of (ARCHITECTURE.md §5,
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

// Capability identifies a scoped grant a goober may hold and a task or gate
// may declare use of. The dotted "resource:verb" shape mirrors provider/
// credential capability strings already in use (ARCHITECTURE.md §5).
type Capability string

// The canonical V0 capability set — every capability string that actually
// appears in a shipped or example workflow/goober definition
// (internal/workflow/testdata/shipped, config-examples/). Add new entries
// here first; the compiler rejects anything not listed (WithGoobers-gated
// admission, internal/workflow/compile.go), so a definition can never drift
// ahead of this registry.
const (
	// RepoRead grants a read-only checkout of the target repository's per-stage
	// worktree — no push. Added for the work-nomination workflow (issue #26):
	// its nominator goober analyzes the codebase for gaps but writes issues
	// only, never code.
	RepoRead Capability = "repo:read"
	// RepoPush grants `git push` to the target repository's per-stage worktree.
	RepoPush Capability = "repo:push"
	// GitHubIssuesWrite grants GitHub issue query/create/label/close/comment
	// (the backlog-curation and work-nomination workflows' surface).
	GitHubIssuesWrite Capability = "github:issues:write"
	// GitHubPRWrite grants GitHub PR open/poll/close (the implementation
	// workflow's open-pr and ci-poll stages).
	GitHubPRWrite Capability = "github:pr:write"
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
	return []Capability{RepoRead, RepoPush, GitHubIssuesWrite, GitHubPRWrite, GitHubPRMerge, TelemetryRead, JournalRead, AgentModel}
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
