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

		// --- STILL REFUSED: a direct instance-root file no plane serves ---
		// Each of these names the specific file in shell.go's map comment.
		{name: "pr-select (SchedulerDir/pr-select-fairness.json)", cmd: []string{"goobers", "pr-select"}, want: true},
		{name: "issue-close-out (journal.OpenRead over FindRunDir)", cmd: []string{"goobers", "issue-close-out"}, want: true},
		{name: "telemetry-query (layout.TelemetryDB)", cmd: []string{"goobers", "telemetry-query", "--window", "24h"}, want: true},
		{name: "gather-pr-context (SchedulerDir/pr-remediation-noop.json)", cmd: []string{"goobers", "gather-pr-context"}, want: true},
		{name: "select-source (instance log + direct claim ledger)", cmd: []string{"goobers", "select-source"}, want: true},
		{name: "publish-batch (SchedulerDir/decomposition-target-locks)", cmd: []string{"goobers", "publish-batch"}, want: true},
		{name: "publish-batch with an unrelated --claim-shaped flag", cmd: []string{"goobers", "publish-batch", "--claim"}, want: true},
		{name: "reconcile-branches (instance log + RunsDir walk)", cmd: []string{"goobers", "reconcile-branches"}, want: true},

		// --- NO LONGER REFUSED: every stateful access is plane-served
		// (Goobers#3897 stamps the endpoints and bearers; #3898 moved the
		// claiming path's annotation write and re-sweep state onto planes).
		// A regression here means a stage silently takes the local-file
		// branch again, which is the failure both issues were filed about.
		{name: "pr-claim bare (claims plane)", cmd: []string{"goobers", "pr-claim"}, want: false},
		{name: "pr-claim --release (claims plane)", cmd: []string{"goobers", "pr-claim", "--release"}, want: false},
		{name: "update-behind-pr (claims plane)", cmd: []string{"goobers", "update-behind-pr"}, want: false},
		{name: "merge-pr (claims plane, incl. its merge lock)", cmd: []string{"goobers", "merge-pr"}, want: false},
		{name: "reconcile-post-merge (scheduler-state plane)", cmd: []string{"goobers", "reconcile-post-merge"}, want: false},
		{name: "apply-verdict (journal read plane)", cmd: []string{"goobers", "apply-verdict"}, want: false},
		{name: "respond-to-findings plain (journal read plane)", cmd: []string{"goobers", "respond-to-findings"}, want: false},
		{name: "respond-to-findings --check (validate-finding-responses)", cmd: []string{"goobers", "respond-to-findings", "--check"}, want: false},
		{name: "gather-implement-context (cross-run journal plane)", cmd: []string{"goobers", "gather-implement-context"}, want: false},
		{name: "backlog-dedupe (claims plane)", cmd: []string{"goobers", "backlog-dedupe"}, want: false},
		{name: "gather-ci-failures (journal read plane)", cmd: []string{"goobers", "gather-ci-failures"}, want: false},
		{name: "gather-issue-context (journal read plane)", cmd: []string{"goobers", "gather-issue-context"}, want: false},
		{name: "gather-sibling-context (scheduler-state plane)", cmd: []string{"goobers", "gather-sibling-context", "--no-verdict-cache"}, want: false},
		{name: "resolve-review-threads (journal read plane)", cmd: []string{"goobers", "resolve-review-threads"}, want: false},
		{name: "post-merge (scheduler-state plane)", cmd: []string{"goobers", "post-merge"}, want: false},
		{name: "validate-plan (journal read plane)", cmd: []string{"goobers", "validate-plan"}, want: false},
		{name: "gate-removal-guard (journal read plane)", cmd: []string{"goobers", "gate-removal-guard"}, want: false},

		// --- flag-gated commands ---
		// backlog-query is Goobers#3898's subject: its scan cursor and
		// re-sweep state are scheduler-state keys, its claims are the claims
		// plane, and its blocked-eligibility annotations go over the journal
		// emit plane. Every mode is now dispatchable, including the ones the
		// flag gate used to single out.
		{name: "backlog-query --claim", cmd: []string{"goobers", "backlog-query", "--claim"}, want: false},
		{name: "backlog-query --release", cmd: []string{"goobers", "backlog-query", "--release"}, want: false},
		{name: "backlog-query --reconcile", cmd: []string{"goobers", "backlog-query", "--reconcile"}, want: false},
		{name: "backlog-query --debug --claim", cmd: []string{"goobers", "backlog-query", "--debug", "--claim"}, want: false},
		{name: "backlog-query bare", cmd: []string{"goobers", "backlog-query"}, want: false},
		{name: "backlog-query --debug alone", cmd: []string{"goobers", "backlog-query", "--debug"}, want: false},
		{name: "backlog-query --read-only", cmd: []string{"goobers", "backlog-query", "--read-only"}, want: false},
		// backlog-health stays refused in EVERY mode, and for a reason that
		// survived both issues: its resumable ready-transition ledger
		// (layout.BacklogHealthCursorPath) is not a scheduler-state key.
		{name: "backlog-health --feedback", cmd: []string{"goobers", "backlog-health", "--feedback"}, want: true},
		{name: "backlog-health bare", cmd: []string{"goobers", "backlog-health"}, want: true},

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
