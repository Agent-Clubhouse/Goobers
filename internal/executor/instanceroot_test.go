package executor

import "testing"

// TestStageRequiresInstanceRoot is decision 003 ruling 3's table test over
// the one exported refusal list: every command the production-lanes-3.0
// inventory names as ledger-touching, journal-reading, or telemetry-rollup-
// reading, plus every built-in kind WITHOUT a pod-side execution path, must
// be refused; every read-only or unrelated command — and, since #3881, the
// ci-poll kind that now has one — must not.
func TestStageRequiresInstanceRoot(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		kind string
		want bool
	}{
		// --- kind-based refusals: an unrecognized kind has no pod-side path ---
		// ci-poll LEFT this list (decision 005 C5, #3881): dispatch-exec runs
		// executor.CIPollExecutor in-process in the pod
		// (cmd/goobers/dispatchcipoll.go) with provider:pr:write resolved from
		// the credential plane, so the kind no longer needs the daemon's
		// instance root. external-telemetry stays: its executor is built from
		// the instance's connector configuration, which lives under a config
		// directory a stage pod does not have.
		{name: "ci-poll kind runs in a pod", cmd: []string{"goobers", "ci-poll"}, kind: "ci-poll", want: false},
		{name: "external-telemetry kind", cmd: []string{"goobers", "external-telemetry"}, kind: "external-telemetry", want: true},
		// The allowlist direction, stated as a test: a kind this binary has
		// never heard of (a newer engine dispatching to an older pod image) is
		// refused rather than dispatched into a pod that would silently run the
		// stage's placeholder command instead of the kind.
		{name: "an unrecognized kind is refused", cmd: []string{"goobers", "some-future-kind"}, kind: "some-future-kind", want: true},
		{name: "explicit shell kind falls through to command", cmd: []string{"goobers", "push-branch"}, kind: "shell", want: false},
		{name: "empty kind falls through to command", cmd: []string{"make", "ci"}, kind: "", want: false},
		// Kind admission does not launder the COMMAND check: a ci-poll kind
		// declared over a ledger-touching command is still refused, so the
		// allowlist cannot become a way to smuggle one past the list below.
		// (merge-pr was this case's subject until Goobers#3897/#3898; it is
		// now plane-served end to end, so the exemplar moved to a command
		// that still holds an instance-root file.)
		{name: "ci-poll kind over a ledger command is still refused", cmd: []string{"goobers", "select-source"}, kind: "ci-poll", want: true},

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
		// backlog-health joined them at Goobers#3948: its ready-transition
		// ledger (layout.BacklogHealthCursorPath) is a scheduler-state key
		// now, so neither mode holds it to the instance root any more.
		{name: "backlog-health --feedback", cmd: []string{"goobers", "backlog-health", "--feedback"}, want: false},
		{name: "backlog-health bare", cmd: []string{"goobers", "backlog-health"}, want: false},

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
