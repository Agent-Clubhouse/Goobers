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
	// TelemetryRead grants read access to the local telemetry rollup
	// (stats/errors/spans) — internal/telemetry/query's gate.
	TelemetryRead Capability = "telemetry:read"
)

// All returns every canonical capability, in declaration order.
func All() []Capability {
	return []Capability{RepoRead, RepoPush, GitHubIssuesWrite, GitHubPRWrite, TelemetryRead}
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
