package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestPinnedHandoffLossIsDistinguishableFromAnIdleCycle is #3098's diagnostic
// contract.
//
// update-behind-pr selected PR #567 with needsFullRemediation=true;
// gather-pr-context reported no-work, and the run completed successfully
// without rebase-pr, gather-ci-failures or the agent ever running. Two
// consecutive runs reproduced it identically and the failing CI the run existed
// to fix was never handed to remediation. The reason it stayed invisible is
// that the message was the same one an idle tick prints.
func TestPinnedHandoffLossIsDistinguishableFromAnIdleCycle(t *testing.T) {
	pinned := []providers.PullRequestSummary{{Number: 567, Head: "goobers/implementation/e453c429"}}

	t.Run("branch held by another in-flight run", func(t *testing.T) {
		got := pinnedHandoffLossReason(pinned, map[string]bool{"goobers/implementation/e453c429": true})

		if !strings.Contains(got, "#567") {
			t.Fatalf("reason = %q, want it to name the lost pull request", got)
		}
		// The deferral is deliberate (#872/#1007): colliding on checkout is
		// worse. What was missing is saying so.
		if !strings.Contains(got, "deferred") || !strings.Contains(got, "worktree") {
			t.Fatalf("reason = %q, want it to name the held branch as the cause", got)
		}
		if !strings.Contains(got, "not an idle cycle") {
			t.Fatalf("reason = %q, want it to separate this from an idle tick", got)
		}
	})

	t.Run("dropped for some other eligibility reason", func(t *testing.T) {
		got := pinnedHandoffLossReason(pinned, nil)

		if !strings.Contains(got, "#567") || !strings.Contains(got, "handoff loss") {
			t.Fatalf("reason = %q, want a handoff loss naming the pull request", got)
		}
		if strings.Contains(got, "worktree") {
			t.Fatalf("reason = %q, want no held-branch claim when no branch is held", got)
		}
	})

	t.Run("no pinned identity recovered", func(t *testing.T) {
		got := pinnedHandoffLossReason(nil, nil)
		if !strings.Contains(got, "handoff loss") {
			t.Fatalf("reason = %q, want it to still read as a handoff loss", got)
		}
	})
}

// TestSelectCandidateReportsTheLostHandoff drives the real decision rather
// than the message helper: gatherPRContextSelectCandidate must reach the new
// branch when this run arrived with a pinned candidate that did not survive
// eligibility. Without this, the helper could be correct and unreachable.
//
// Only the pinned branch is exercised here; the unpinned branch runs the claim
// protocol and needs a provisioned instance, and the existing suite already
// covers its no-work reason.
func TestSelectCandidateReportsTheLostHandoff(t *testing.T) {
	// The CLI entrypoint writes worktree-relative result files (#3459).
	t.Chdir(t.TempDir())
	pinnedPR := providers.PullRequestSummary{Number: 567, Head: "goobers/implementation/e453c429"}

	var stdout, stderr strings.Builder
	_, done, code := gatherPRContextSelectCandidate(gatherPRContextCandidateSelection{
		root: t.TempDir(), hasPinnedCandidate: true,
		eligibleInput: []providers.PullRequestSummary{pinnedPR},
		heldBranches:  map[string]bool{pinnedPR.Head: true},
	}, &stdout, &stderr)

	if !done || code != 0 {
		t.Fatalf("done = %v, code = %d, stderr = %q; want a clean no-work", done, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "#567") || !strings.Contains(stdout.String(), "not an idle cycle") {
		t.Fatalf("stdout = %q, want the lost-handoff reason naming the pull request", stdout.String())
	}
	if strings.Contains(stdout.String(), "no PR needs remediation this cycle") {
		t.Fatalf("stdout = %q, want the specific reason, not the idle-cycle one", stdout.String())
	}
}
