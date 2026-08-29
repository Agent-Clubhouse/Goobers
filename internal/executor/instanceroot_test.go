package executor

import "testing"

// TestStageRequiresInstanceRoot is decision 003 ruling 3's table test over
// the one exported refusal list: every command the production-lanes-3.0
// inventory names as ledger-touching, journal-reading, or telemetry-rollup-
// reading, plus the two non-shell built-in kinds, must be refused; every
// read-only or unrelated command must not.
func TestStageRequiresInstanceRoot(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		kind string
		want bool
	}{
		// --- kind-based refusals: Run.Kind != shell ---
		{name: "ci-poll kind", cmd: []string{"goobers", "ci-poll"}, kind: "ci-poll", want: true},
		{name: "external-telemetry kind", cmd: []string{"goobers", "external-telemetry"}, kind: "external-telemetry", want: true},
		{name: "explicit shell kind falls through to command", cmd: []string{"goobers", "push-branch"}, kind: "shell", want: false},
		{name: "empty kind falls through to command", cmd: []string{"make", "ci"}, kind: "", want: false},

		// --- unconditional ledger/journal/telemetry commands ---
		{name: "pr-claim bare", cmd: []string{"goobers", "pr-claim"}, want: true},
		{name: "pr-claim --release", cmd: []string{"goobers", "pr-claim", "--release"}, want: true},
		{name: "pr-select", cmd: []string{"goobers", "pr-select"}, want: true},
		{name: "update-behind-pr", cmd: []string{"goobers", "update-behind-pr"}, want: true},
		{name: "merge-pr", cmd: []string{"goobers", "merge-pr"}, want: true},
		{name: "reconcile-post-merge", cmd: []string{"goobers", "reconcile-post-merge"}, want: true},
		{name: "apply-verdict", cmd: []string{"goobers", "apply-verdict"}, want: true},
		{name: "respond-to-findings plain", cmd: []string{"goobers", "respond-to-findings"}, want: true},
		{name: "respond-to-findings --check (validate-finding-responses)", cmd: []string{"goobers", "respond-to-findings", "--check"}, want: true},
		{name: "gather-implement-context", cmd: []string{"goobers", "gather-implement-context"}, want: true},
		{name: "issue-close-out", cmd: []string{"goobers", "issue-close-out"}, want: true},
		{name: "telemetry-query", cmd: []string{"goobers", "telemetry-query", "--window", "24h"}, want: true},
		{name: "backlog-dedupe", cmd: []string{"goobers", "backlog-dedupe"}, want: true},
		{name: "gather-pr-context", cmd: []string{"goobers", "gather-pr-context"}, want: true},
		{name: "gather-ci-failures", cmd: []string{"goobers", "gather-ci-failures"}, want: true},
		{name: "gather-issue-context", cmd: []string{"goobers", "gather-issue-context"}, want: true},
		{name: "gather-sibling-context", cmd: []string{"goobers", "gather-sibling-context", "--no-verdict-cache"}, want: true},
		{name: "resolve-review-threads", cmd: []string{"goobers", "resolve-review-threads"}, want: true},
		{name: "select-source", cmd: []string{"goobers", "select-source"}, want: true},
		{name: "publish-batch", cmd: []string{"goobers", "publish-batch"}, want: true},
		{name: "publish-batch with an unrelated --claim-shaped flag", cmd: []string{"goobers", "publish-batch", "--claim"}, want: true},
		{name: "post-merge", cmd: []string{"goobers", "post-merge"}, want: true},
		{name: "reconcile-branches", cmd: []string{"goobers", "reconcile-branches"}, want: true},
		{name: "validate-plan", cmd: []string{"goobers", "validate-plan"}, want: true},
		{name: "gate-removal-guard", cmd: []string{"goobers", "gate-removal-guard"}, want: true},

		// --- flag-gated commands ---
		{name: "backlog-query --claim", cmd: []string{"goobers", "backlog-query", "--claim"}, want: true},
		{name: "backlog-query --release", cmd: []string{"goobers", "backlog-query", "--release"}, want: true},
		{name: "backlog-query --reconcile", cmd: []string{"goobers", "backlog-query", "--reconcile"}, want: true},
		{name: "backlog-query --debug --claim", cmd: []string{"goobers", "backlog-query", "--debug", "--claim"}, want: true},
		{name: "backlog-query bare reaches the scan lock", cmd: []string{"goobers", "backlog-query"}, want: true},
		{name: "backlog-query --debug alone reaches the scan lock", cmd: []string{"goobers", "backlog-query", "--debug"}, want: true},
		{name: "backlog-query --read-only", cmd: []string{"goobers", "backlog-query", "--read-only"}, want: false},
		{name: "backlog-health --feedback", cmd: []string{"goobers", "backlog-health", "--feedback"}, want: true},
		{name: "backlog-health bare is read-only", cmd: []string{"goobers", "backlog-health"}, want: false},

		// --- unrelated / provider-only commands stay dispatchable ---
		{name: "push-branch", cmd: []string{"goobers", "push-branch"}, want: false},
		{name: "open-pr", cmd: []string{"goobers", "open-pr"}, want: false},
		{name: "make ci is not a goobers CLI stage", cmd: []string{"make", "ci"}, want: false},
		{name: "empty command", cmd: nil, want: false},
		{name: "goobers with no subcommand", cmd: []string{"goobers"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StageRequiresInstanceRoot(tc.cmd, tc.kind); got != tc.want {
				t.Fatalf("StageRequiresInstanceRoot(%v, %q) = %v, want %v", tc.cmd, tc.kind, got, tc.want)
			}
		})
	}
}

// TestCommandDeclaresAnyFlag exercises the flag-form matching
// StageRequiresInstanceRoot relies on for backlog-query/backlog-health: both
// single- and double-dash spellings, and the "=value" form.
func TestCommandDeclaresAnyFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{name: "double dash", args: []string{"--claim"}, want: true},
		{name: "single dash", args: []string{"-claim"}, want: true},
		{name: "equals form", args: []string{"--claim=true"}, want: true},
		{name: "among other flags", args: []string{"--debug", "--claim"}, want: true},
		{name: "no match", args: []string{"--debug", "--read-only"}, want: false},
		{name: "empty", args: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandDeclaresAnyFlag(tc.args, "claim", "release", "reconcile"); got != tc.want {
				t.Fatalf("commandDeclaresAnyFlag(%v, claim/release/reconcile) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
