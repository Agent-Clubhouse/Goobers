package main

import (
	"strings"
	"testing"
	"time"
)

// #6134 follow-up: an armed landing (an Azure DevOps auto-complete set while a
// blocking policy was pending) must not complete over human feedback that
// arrived after it was armed. pr-comment-watch scans merge-ready PRs, cancels
// the auto-complete, and only then routes the PR to remediation.
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
	})
	runADOCommentWatch(t, root)
	writes := fake.takeWrites()
	want := []string{"disarm:1", "remove:1:" + mergeReadyLabel, "add:1:" + needsRemediationLabel}
	if strings.Join(writes, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want %v (disarm before routing; the quiet PR untouched)", writes, want)
	}
	fake.mu.Lock()
	armed, quiet := fake.prs[1].autoComplete, fake.prs[2].autoComplete
	fake.mu.Unlock()
	if armed || !quiet {
		t.Fatalf("auto-complete armed: PR 1 = %v, PR 2 = %v; want only PR 1 disarmed", armed, quiet)
	}
	result := readCommentWatchResult(t, dir)
	if result.Labeled != 1 || len(result.PRs) != 1 || !result.PRs[0].Disarmed || result.PRs[0].Unparked {
		t.Fatalf("result = %+v, want PR 1 routed and reported disarmed (not un-parked)", result)
	}
}

// On GitHub the watcher routes a merge-ready PR (merge-ready removed,
// needs-remediation added); queue-watch, which holds the merge authority while
// the PR is queued, dequeues it on that label.
func TestPRCommentWatchRoutesMergeReadyPRGitHub(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	base := time.Now().UTC().Add(-2 * time.Hour)
	server.addOpenPR(1, "goobers/implementation/x", "main", "sha", "base", false, []string{mergeReadyLabel}, nil)
	server.addIssue(1, "Landing", mergeReadyLabel)
	server.addCommentAtAs(1, "goobers", "on it", base)
	server.addCommentAtAs(1, "gneitzke", "please fix", base.Add(time.Hour))
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	dir := commentWatchWorkDir(t)

	if code, _, stderr := runArgs(t, "pr-comment-watch", root); code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stderr = %q", code, stderr)
	}
	if !server.issueHasLabel(1, needsRemediationLabel) || server.issueHasLabel(1, mergeReadyLabel) {
		t.Fatalf("labels: want %s added and %s removed", needsRemediationLabel, mergeReadyLabel)
	}
	if result := readCommentWatchResult(t, dir); result.Labeled != 1 || !result.PRs[0].Disarmed {
		t.Fatalf("result = %+v, want the landing PR routed", result)
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
	if result := readQueueResult(t, dir); result["queueOutcome"] != mergeReviewOptOutOutcome {
		t.Fatalf("result = %+v, want the queued PR withdrawn", result)
	}
	if st.dequeueCalls != 1 {
		t.Fatalf("dequeue calls = %d, want 1 once the PR was routed to remediation", st.dequeueCalls)
	}
}
