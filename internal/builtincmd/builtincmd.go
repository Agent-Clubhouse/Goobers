// Package builtincmd is the authoritative inventory of built-in `goobers`
// subcommand names a workflow's run.command may invoke as
// ["goobers", "<name>", ...] when the stage actually shells out — i.e. its
// inputs.kind is empty or "shell". The DSL compilers (internal/workflow's
// v_current and v_next admissionProblems) reject a shell-out to a name
// outside this inventory, because such a stage fails at runtime with the
// CLI's own "unknown command" only after the run has already been scheduled,
// claimed work, and provisioned a worktree.
//
// The inventory is deliberately WIDER than internal/providerstage's manifest
// (which only describes commands with declared capability requirements):
// shipped/reference workflows also shell out to stage commands with no
// capability manifest entry (check-fail-first, gate-removal-guard,
// docs-churn, ios-simulator-test, mcp-io) and to two non-stage commands
// (validate in the tutor workflow, self-update in the self-update workflow).
//
// It is deliberately NARROWER than the CLI's full command surface: names
// like `up`, `status`, or `init` are operator commands, not stage
// invocations, and a workflow shelling out to them is almost certainly a
// config error.
//
// Kind-backed placeholder commands (["goobers", "ci-poll"] with
// inputs.kind: ci-poll, ["goobers", "external-telemetry"] with
// inputs.kind: external-telemetry) are NOT listed: those stages never shell
// out — the runner dispatches on inputs.kind and the command is a schema-
// required placeholder — so the compilers exempt any stage with a non-shell
// inputs.kind before consulting this inventory at all.
//
// This package is a data-only list: cmd/goobers's cliCommands registry is
// this repo's declared source of truth for the command surface, but it lives
// in package main and cannot be imported. The inventory therefore DEFERS to
// it rather than forking it — a parity test in cmd/goobers asserts every
// name here resolves in the CLI registry (the inventory cannot invent
// commands), and a companion test asserts every providerstage.Commands()
// name is present here (the inventory cannot silently lag the manifest).
// This package has no dependencies beyond the stdlib so both interpreters
// and cmd/goobers tests can import it freely.
package builtincmd

// names is the inventory, sorted. Keep it sorted: tests enforce order and
// uniqueness so diffs stay reviewable and lookups can stay simple.
var names = []string{
	// The offline demo provider (`goobers init --demo`): the seeded demo
	// gaggle's workflow stages shell out to it by design.
	"__demo-provider",
	"apply-verdict",
	"backlog-assignment",
	"backlog-dedupe",
	"backlog-health",
	"backlog-query",
	"check-fail-first",
	"check-issue-staleness",
	"docs-churn",
	"elect-lander",
	"gate-removal-guard",
	"gather-ci-failures",
	"gather-implement-context",
	"gather-issue-context",
	"gather-pr-context",
	"gather-review-threads",
	"gather-sibling-context",
	"ios-simulator-test",
	"issue-close-out",
	"mcp-io",
	"merge-pr",
	"merge-queue-poll",
	"open-pr",
	"post-merge",
	"pr-claim",
	"pr-select",
	"publish-batch",
	"push-branch",
	"push-remediated",
	"rebase-pr",
	"reconcile-branches",
	"reconcile-post-merge",
	"record-merge-refusal",
	"remediation-checkpoint",
	"report-pr-status",
	"resolve-review-threads",
	"respond-to-findings",
	"select-source",
	"self-update",
	"set-milestone",
	"telemetry-query",
	"update-behind-pr",
	"validate",
	"validate-plan",
}

// Names returns every inventoried subcommand name, sorted. The result is a
// copy; callers may not mutate the inventory.
func Names() []string {
	return append([]string(nil), names...)
}

// Known reports whether name is an inventoried built-in subcommand.
func Known(name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// Suggest returns the closest inventoried subcommand for a likely typo,
// mirroring internal/capability.Suggest: the nearest name by edit distance,
// but only when it is close enough (distance <= 2) to plausibly be what the
// author meant.
func Suggest(name string) (string, bool) {
	if Known(name) {
		return "", false
	}
	bestDistance := -1
	best := ""
	for _, candidate := range names {
		distance := editDistance(name, candidate)
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
