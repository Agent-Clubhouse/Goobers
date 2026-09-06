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
		// instance root. external-telemetry LEFT it too (#4341): the
		// dispatcher stamps the one named connector's non-secret config and
		// dispatch-exec resolves its auth secret from the credential plane,
		// exactly as ci-poll resolves its provider token.
		{name: "ci-poll kind runs in a pod", cmd: []string{"goobers", "ci-poll"}, kind: "ci-poll", want: false},
		{name: "external-telemetry kind runs in a pod", cmd: []string{"goobers", "external-telemetry"}, kind: "external-telemetry", want: false},
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
		// that still holds an instance-root file. select-source moved off this
		// list at Goobers#4342, so the exemplar moved again, to a command that
		// still holds one.)
		{name: "ci-poll kind over a ledger command is still refused", cmd: []string{"goobers", "reconcile-branches"}, kind: "ci-poll", want: true},

		// --- STILL REFUSED: a direct instance-root file no plane serves ---
		// Each of these names the specific file in shell.go's map comment.
		{name: "issue-close-out (journal read plane)", cmd: []string{"goobers", "issue-close-out"}, want: false},
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
		// publish-batch is Goobers#4340's subject: its target lease is now a
		// distinctly-keyed claims-plane lease (claimsPlaneTargetLeaser) instead
		// of a FileTargetLeaser over a local-only lock directory, and its
		// parent claim release goes through the same claims-plane seam instead
		// of a direct file-ledger open.
		{name: "publish-batch (claims plane target lease + parent release)", cmd: []string{"goobers", "publish-batch"}, want: false},
		{name: "publish-batch with an unrelated --claim-shaped flag", cmd: []string{"goobers", "publish-batch", "--claim"}, want: false},
		// select-source is Goobers#4342's subject: its escalation scan is now
		// the cross-run journal plane's fourth question
		// (EscalationCandidates) instead of a direct RunsDir walk, and its
		// parent claim/release go through openStageClaimLedger.
		{name: "select-source (journal plane scan + claims plane claim/release)", cmd: []string{"goobers", "select-source"}, want: false},
		// gather-pr-context is Goobers#3989's subject, and the only entry that
		// needed THREE seams at once: its remediation no-op record is a keyed
		// scheduler-state key, its PR-claim resolution is the claims plane, and
		// its terminal-run journal read is the C4 stageRunJournal seam. A
		// regression here silently fails the no-op guard OPEN in a pod, which
		// loops the lane on the same PR — correctness, not cost.
		{name: "gather-pr-context (state + claims + journal planes)", cmd: []string{"goobers", "gather-pr-context"}, want: false},
		// remediation-checkpoint shares that guard and was never on the list;
		// it is pinned here so the pair cannot drift apart.
		{name: "remediation-checkpoint (shares the no-op guard)", cmd: []string{"goobers", "remediation-checkpoint"}, want: false},

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
		// pr-select joined them at Goobers#3988: its FAIRNESS LEASE
		// (SchedulerDir/pr-select-fairness.json, #1336's aging plus the
		// one-hour starvation guard) is a scheduler-state key now
		// (stateclient.KeyPRSelectFairness), served under the same claims.lock
		// it always took, so a pod and the daemon advance ONE lease. This was
		// the last Self pin on merge-review (M3 of #3828).
		{name: "pr-select (fairness lease on the scheduler-state plane)", cmd: []string{"goobers", "pr-select"}, want: false},
		// telemetry-query joined them at Goobers#4001 (blocker 1 of #3996):
		// its rollup read is a NARROW derived-aggregate plane now
		// (apicontract.TelemetryDefectAggregatesPath) — four fixed families,
		// gaggle-contained, with error signatures normalized before they
		// leave the daemon — and the command selects that plane before it
		// resolves a root at all. A regression here silently returns the
		// defect-nomination lane to a pod-local "." rollup that reports no
		// defects, which is correctness, not cost.
		{name: "telemetry-query (defect-aggregate plane)", cmd: []string{"goobers", "telemetry-query", "--window", "24h"}, want: false},

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
