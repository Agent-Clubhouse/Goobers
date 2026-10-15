package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
)

// #6134 follow-up: an armed landing (an Azure DevOps auto-complete set while a
// blocking policy was pending) must not complete over human feedback that
// arrived after it was armed. With blockUnansweredFeedback "true",
// pr-comment-watch scans merge-ready PRs, cancels the auto-complete, and only
// then routes the PR to remediation.
func TestPRCommentWatchADODisarmsArmedAutoCompleteOnFreshFeedback(t *testing.T) {
	root, fake, dir := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		armed := f.addPR(1, "goobers/implementation/a", mergeReadyLabel)
		armed.autoComplete = true
		armed.threads = []map[string]any{
			adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, ownMarked("verdict"), 0)),
			adoWatchThread(2, adoWatchComment(1, adoWatchHumanID, "Please add a README note first.", 5)),
		}
		quiet := f.addPR(2, "goobers/implementation/b", mergeReadyLabel)
		quiet.autoComplete = true
		quiet.threads = []map[string]any{adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, ownMarked("verdict"), 0))}
		// A reviewer who resolved their own thread is not open feedback:
		// the gate would not refuse, so the landing is not cancelled.
		resolved := f.addPR(3, "goobers/implementation/c", mergeReadyLabel)
		resolved.autoComplete = true
		thread := adoWatchThread(2, adoWatchComment(1, adoWatchHumanID, "nit, never mind", 5))
		thread["status"] = "fixed"
		resolved.threads = []map[string]any{
			adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, ownMarked("verdict"), 0)), thread,
		}
	})
	t.Setenv(executor.InputEnvVar("blockUnansweredFeedback"), "true")
	runADOCommentWatch(t, root)
	writes := fake.takeWrites()
	want := []string{"disarm:1", "remove:1:" + mergeReadyLabel, "add:1:" + needsRemediationLabel}
	if strings.Join(writes, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want %v (disarm before routing; quiet and resolved PRs untouched)", writes, want)
	}
	fake.mu.Lock()
	armed, quiet, resolved := fake.prs[1].autoComplete, fake.prs[2].autoComplete, fake.prs[3].autoComplete
	fake.mu.Unlock()
	if armed || !quiet || !resolved {
		t.Fatalf("auto-complete armed: PR 1 = %v, PR 2 = %v, PR 3 = %v; want only PR 1 disarmed", armed, quiet, resolved)
	}
	result := readCommentWatchResult(t, dir)
	if result.Labeled != 1 || len(result.PRs) != 1 || !result.PRs[0].Disarmed || result.PRs[0].Unparked {
		t.Fatalf("result = %+v, want PR 1 routed and reported disarmed (not un-parked)", result)
	}
}

// Disarming is opt-in (it is a new mutation the task must declare as
// disarm-landing): by default a landing PR is skipped exactly as before.
func TestPRCommentWatchSkipsLandingPRsByDefault(t *testing.T) {
	root, fake, dir := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		armed := f.addPR(1, "goobers/implementation/a", mergeReadyLabel)
		armed.autoComplete = true
		armed.threads = []map[string]any{adoWatchThread(2, adoWatchComment(1, adoWatchHumanID, "Please add a README note first.", 5))}
	})
	runADOCommentWatch(t, root)
	if writes := fake.takeWrites(); len(writes) != 0 {
		t.Fatalf("writes = %v, want none without blockUnansweredFeedback", writes)
	}
	if result := readCommentWatchResult(t, dir); result.Scanned != 0 {
		t.Fatalf("result = %+v, want the landing PR skipped", result)
	}
}

// On GitHub the watcher routes a merge-ready PR (merge-ready removed,
// needs-remediation added) but disarms nothing itself: queue-watch, which
// holds the merge authority while the PR is queued, withdraws it.
func TestPRCommentWatchRoutesMergeReadyPRGitHub(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	base := time.Now().UTC().Add(-2 * time.Hour)
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha", "base", false, []string{mergeReadyLabel}, nil)
	server.addIssue(1, "Landing", mergeReadyLabel)
	server.addCommentAtAs(1, "goobers", "on it", base)
	server.addCommentAtAs(1, "gneitzke", "please fix", base.Add(time.Hour))
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv(executor.InputEnvVar("blockUnansweredFeedback"), "true")
	dir := commentWatchWorkDir(t)

	if code, _, stderr := runArgs(t, "pr-comment-watch", root); code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	if !server.issueHasLabel(1, needsRemediationLabel) || server.issueHasLabel(1, mergeReadyLabel) {
		t.Fatalf("labels: want %s added and %s removed", needsRemediationLabel, mergeReadyLabel)
	}
	if result := readCommentWatchResult(t, dir); result.Labeled != 1 || result.PRs[0].Disarmed {
		t.Fatalf("result = %+v, want the landing PR routed and not reported disarmed", result)
	}
}

func TestMergeQueuePollDequeuesPRRoutedToRemediation(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript:           []string{"pending", "pending"},
		pendingCalls:            1_000_000,
		terminalState:           "open",
		labels:                  []string{needsRemediationLabel},
		optOutAfterGraphQLPolls: 1,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if result["queueOutcome"] != mergeReviewOptOutOutcome {
		t.Fatalf("result = %+v, want the queued PR withdrawn", result)
	}
	if reason, _ := result["reason"].(string); !strings.Contains(reason, "labeled "+needsRemediationLabel+" ") {
		t.Fatalf("reason = %q, want the withdrawing label named", reason)
	}
	if st.dequeueCalls != 1 {
		t.Fatalf("dequeue calls = %d, want 1 once the PR was routed to remediation", st.dequeueCalls)
	}
}

// post-merge's sibling fan-out labels a queued sibling needs-remediation when
// it merely overlaps a landed PR's files, leaving merge-ready in place: that
// PR stays queued and lands through the queue's combined build.
func TestMergeQueuePollKeepsQueuedSiblingLabeledByPostMerge(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript:           []string{"pending", "pending", "merged"},
		pendingCalls:            1_000_000,
		terminalState:           "closed",
		terminalMerged:          true,
		labels:                  []string{mergeReadyLabel, needsRemediationLabel},
		optOutAfterGraphQLPolls: 1,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if result := readQueueResult(t, dir); result["queueOutcome"] != "merged" || st.dequeueCalls != 0 {
		t.Fatalf("result = %+v (dequeue calls %d), want the merge-ready sibling kept queued and merged", result, st.dequeueCalls)
	}
}

// On Azure DevOps a withdrawal (here: routed to remediation off merge-ready)
// cancels the armed auto-complete, closing the window in which a PR routed
// for feedback could still complete.
func TestMergeQueuePollADOWithdrawalCancelsAutoComplete(t *testing.T) {
	st := &adoPRDetailState{status: "active", autoComplete: true, labels: []string{needsRemediationLabel}}
	server := newADOMergeQueuePollServer(t, "acme", "proj", "svc", st)
	root, dir := adoMergeQueuePollEnv(t, server.URL, "acme", "proj", "svc", true, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	st.mu.Lock()
	cancels, armed := st.cancels, st.autoComplete
	st.mu.Unlock()
	if cancels != 1 || armed {
		t.Fatalf("auto-complete cancels = %d, still armed = %v; want one cancel", cancels, armed)
	}
	if result := readQueueResult(t, dir); result["queueOutcome"] != mergeReviewOptOutOutcome {
		t.Fatalf("result = %+v, want the PR withdrawn", result)
	}
}
