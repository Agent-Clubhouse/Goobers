package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

// #6134: queue-watch runs #7409's unanswered-feedback check on every poll of a
// queued PR. Feedback that arrives after the landing was armed withdraws it
// (GitHub: dequeue) and routes it to remediation with the typed reason.
func TestMergeQueuePollWithdrawsQueuedPROnUnansweredFeedbackGitHub(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript: []string{"pending"},
		labels:        []string{mergeReadyLabel},
		issueComments: []map[string]interface{}{{
			"id": 31, "body": "Please add a README note before this lands.", "created_at": "2026-10-01T12:00:00Z",
			"user": map[string]string{"login": "alice", "type": "User"},
		}},
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	reason, _ := result["reason"].(string)
	if result["queueOutcome"] != mergeReviewOptOutOutcome || !strings.HasPrefix(reason, unansweredHumanFeedbackReasonCode+":") {
		t.Fatalf("result = %+v, want the PR withdrawn with the typed feedback reason", result)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dequeueCalls != 1 {
		t.Fatalf("dequeue calls = %d, want 1", st.dequeueCalls)
	}
	if st.labelCalls == 0 || len(st.commentBodies) != 1 || !goobersBookkeepingComment(st.commentBodies[0]) ||
		!strings.Contains(st.commentBodies[0], feedbackRefusalPayloadPrefix) {
		t.Fatalf("labels %d, comments %q; want routed through the refusal record (bookkeeping, acknowledges nothing)", st.labelCalls, st.commentBodies)
	}
}

// The knob merge-pr honours: blockUnansweredFeedback "false" lets a queued PR
// land over comments.
func TestMergeQueuePollFeedbackCheckOptOutGitHub(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript: []string{"pending", "pending", "merged"},
		labels:        []string{mergeReadyLabel},
		issueComments: []map[string]interface{}{{
			"id": 31, "body": "Please add a README note.", "created_at": "2026-10-01T12:00:00Z",
			"user": map[string]string{"login": "alice", "type": "User"},
		}},
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	t.Setenv(executor.InputEnvVar("blockUnansweredFeedback"), "false")
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if result := readQueueResult(t, dir); result["queueOutcome"] != "merged" || st.dequeueCalls != 0 {
		t.Fatalf("result = %+v (dequeues %d), want it landed with the check opted out", result, st.dequeueCalls)
	}
}

// A feedback read that fails keeps the PR queued and retries next poll; one
// failed read is not evidence of unanswered feedback.
func TestMergeQueuePollFeedbackReadFailureKeepsQueuedGitHub(t *testing.T) {
	st := &mergeQueuePollServerState{
		graphqlScript:      []string{"pending", "pending", "merged"},
		labels:             []string{mergeReadyLabel},
		feedbackReadStatus: 404,
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	code, _, stderr := runArgs(t, "merge-queue-poll", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "keeping it queued and re-reading on the next poll") {
		t.Fatalf("stderr = %q, want the failed read reported", stderr)
	}
	if result := readQueueResult(t, dir); result["queueOutcome"] != "merged" || st.dequeueCalls != 0 {
		t.Fatalf("result = %+v (dequeues %d), want it kept queued", result, st.dequeueCalls)
	}
}

// Azure DevOps: an active human thread on a PR with an armed auto-complete
// cancels the auto-complete and routes the PR with pull-request labels.
func TestMergeQueuePollWithdrawsArmedAutoCompleteOnUnansweredFeedbackADO(t *testing.T) {
	st := &adoPRDetailState{
		status: "active", autoComplete: true, labels: []string{mergeReadyLabel},
		threads: []map[string]interface{}{{
			"id": 21, "status": "active",
			"comments": []map[string]interface{}{{
				"id": 1, "commentType": "text", "content": "Please add a README note before this lands.",
				"author":        map[string]string{"id": "reviewer-guid", "displayName": "A Reviewer"},
				"publishedDate": "2026-10-01T12:00:00Z",
			}},
		}},
	}
	server := newADOMergeQueuePollServer(t, "acme", "proj", "svc", st)
	root, dir := adoMergeQueuePollEnv(t, server.URL, "acme", "proj", "svc", true, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	result := readQueueResult(t, dir)
	if reason, _ := result["reason"].(string); !strings.HasPrefix(reason, unansweredHumanFeedbackReasonCode+":") {
		t.Fatalf("result = %+v, want the typed feedback reason", result)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cancels != 1 || st.autoComplete {
		t.Fatalf("cancels = %d, armed = %v; want the auto-complete cancelled once", st.cancels, st.autoComplete)
	}
	if !slices.Contains(st.labelWrites, "add:"+needsRemediationLabel) || !slices.Contains(st.labelWrites, "remove:"+mergeReadyLabel) {
		t.Fatalf("label writes = %v, want routed to remediation off merge-ready", st.labelWrites)
	}
	if len(st.workItemHits) != 0 {
		t.Fatalf("work-item calls = %v, want none", st.workItemHits)
	}
}

// A resolved ADO thread is not open feedback: the auto-complete stays armed.
func TestMergeQueuePollKeepsArmedAutoCompleteOverResolvedThreadADO(t *testing.T) {
	st := &adoPRDetailState{
		status: "active", autoComplete: true, labels: []string{mergeReadyLabel},
		threads: []map[string]interface{}{{
			"id": 21, "status": "fixed",
			"comments": []map[string]interface{}{{
				"id": 1, "commentType": "text", "content": "nit, fixed",
				"author":        map[string]string{"id": "reviewer-guid", "displayName": "A Reviewer"},
				"publishedDate": "2026-10-01T12:00:00Z",
			}},
		}},
	}
	server := newADOMergeQueuePollServer(t, "acme", "proj", "svc", st)
	root, _ := adoMergeQueuePollEnv(t, server.URL, "acme", "proj", "svc", true, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "100ms",
	})
	_, _, _ = runArgs(t, "merge-queue-poll", root)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cancels != 0 || !st.autoComplete {
		t.Fatalf("cancels = %d, armed = %v; want the auto-complete left armed", st.cancels, st.autoComplete)
	}
}

// A repeat withdrawal over the SAME comment — remediation ran and still did
// not answer it — parks the PR for a human instead of cycling (#7409's bound
// applies to queue-watch withdrawals too).
func TestMergeQueuePollRepeatWithdrawalParksForHumanGitHub(t *testing.T) {
	alice := map[string]interface{}{
		"id": 31, "body": "Please add a README note before this lands.", "created_at": "2026-10-01T12:00:00Z",
		"user": map[string]string{"login": "alice", "type": "User"},
	}
	record := map[string]interface{}{
		"id": 32, "created_at": "2026-10-01T12:30:00Z", "user": map[string]string{"login": "goobers", "type": "User"},
		"body": stampOwnFixtureBody("Not merging yet.\n\n"+feedbackRefusalPayloadPrefix+`{"comment":"31"} -->`, "comment"),
	}
	st := &mergeQueuePollServerState{
		graphqlScript: []string{"pending"}, labels: []string{mergeReadyLabel},
		issueComments: []map[string]interface{}{alice, record},
	}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, _ := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dequeueCalls != 1 || !slices.Contains(st.addedLabels, "goobers:needs-human") || slices.Contains(st.addedLabels, needsRemediationLabel) {
		t.Fatalf("dequeues %d, added labels %v; want withdrawn and parked needs-human", st.dequeueCalls, st.addedLabels)
	}
}

// A deterministic read failure is not a transient one: an invalid setting on
// queue-watch withdraws the PR as human-feedback-unverifiable and parks it.
func TestMergeQueuePollInvalidFeedbackSettingWithdrawsUnverifiableGitHub(t *testing.T) {
	st := &mergeQueuePollServerState{graphqlScript: []string{"pending"}, labels: []string{mergeReadyLabel}}
	server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
	root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
		"identityMode": "sometimes",
	})
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if reason, _ := readQueueResult(t, dir)["reason"].(string); !strings.HasPrefix(reason, humanFeedbackUnverifiableReasonCode+":") {
		t.Fatalf("reason = %q, want %s", reason, humanFeedbackUnverifiableReasonCode)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dequeueCalls != 1 || !slices.Contains(st.addedLabels, "goobers:needs-human") {
		t.Fatalf("dequeues %d, added labels %v; want withdrawn and parked", st.dequeueCalls, st.addedLabels)
	}
}

// An ADO comment whose author cannot be read withdraws the armed
// auto-complete as unverifiable rather than letting it land unseen.
func TestMergeQueuePollUnreadableCommentWithdrawsUnverifiableADO(t *testing.T) {
	st := &adoPRDetailState{
		status: "active", autoComplete: true, labels: []string{mergeReadyLabel},
		threads: []map[string]interface{}{{
			"id": 21, "status": "active",
			"comments": []map[string]interface{}{{
				"id": 1, "commentType": "text", "content": "Please add a README note.",
				"author": map[string]string{"displayName": "A Reviewer"}, "publishedDate": "2026-10-01T12:00:00Z",
			}},
		}},
	}
	server := newADOMergeQueuePollServer(t, "acme", "proj", "svc", st)
	root, dir := adoMergeQueuePollEnv(t, server.URL, "acme", "proj", "svc", true, map[string]string{
		"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
	})
	if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if reason, _ := readQueueResult(t, dir)["reason"].(string); !strings.HasPrefix(reason, humanFeedbackUnverifiableReasonCode+":") {
		t.Fatalf("reason = %q, want %s", reason, humanFeedbackUnverifiableReasonCode)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cancels != 1 || !slices.Contains(st.labelWrites, "add:goobers:needs-human") {
		t.Fatalf("cancels %d, label writes %v; want the auto-complete cancelled and the PR parked", st.cancels, st.labelWrites)
	}
}
