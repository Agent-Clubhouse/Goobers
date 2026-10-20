package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestMergeQueuePollExplicitParkWithdrawsWithoutFeedback(t *testing.T) {
	for _, label := range []string{providers.LabelNeedsHuman, remediationEscalatedLabel} {
		for _, disabled := range []bool{false, true} {
			name := label + "/feedback-read-error"
			if disabled {
				name = label + "/feedback-disabled"
			}
			t.Run(name, func(t *testing.T) {
				// Keep merge-ready to cover a partial park write too: an explicit human
				// hold must win regardless of when the landing label is removed.
				st := &mergeQueuePollServerState{
					graphqlScript: []string{"pending", "pending", "merged"},
					labels:        []string{label, mergeReadyLabel}, feedbackReadStatus: 404,
				}
				server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
				root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{
					"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
				})
				if disabled {
					t.Setenv(executor.InputEnvVar("blockUnansweredFeedback"), "false")
				}
				code, _, stderr := runArgs(t, "merge-queue-poll", root)
				if code != 0 {
					t.Fatalf("code=%d stderr=%s", code, stderr)
				}
				result := readQueueResult(t, dir)
				st.mu.Lock()
				defer st.mu.Unlock()
				reason, _ := result["reason"].(string)
				if result["queueOutcome"] != mergeReviewOptOutOutcome || st.dequeueCalls != 1 || !strings.Contains(reason, label) {
					t.Fatalf("explicit park was not enforced: result=%+v dequeues=%d", result, st.dequeueCalls)
				}
			})
		}
	}
}

func TestMergeQueuePollBudgetParkAndRemediationRouteWithdrawOnFeedbackReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"existing-remediation-route", []string{needsRemediationLabel}},
		{"new-exhausted-budget-park", []string{providers.LabelNeedsHuman, remediationEscalatedLabel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &mergeQueuePollServerState{graphqlScript: []string{"pending", "pending", "merged"}, labels: tc.labels, feedbackReadStatus: 404}
			server := newMergeQueuePollServer(t, "your-org", "your-repo", st)
			root, dir := mergeQueuePollEnv(t, server.URL, map[string]string{"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s"})
			if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			result := readQueueResult(t, dir)
			if result["queueOutcome"] != mergeReviewOptOutOutcome || st.dequeueCalls != 1 {
				t.Fatalf("parked/routed PR must withdraw: result=%v dequeues=%d", result, st.dequeueCalls)
			}
		})
	}
}

func TestMergeQueuePollExplicitParkDisarmsADOWithoutFeedback(t *testing.T) {
	for _, label := range []string{providers.LabelNeedsHuman, remediationEscalatedLabel} {
		t.Run(label, func(t *testing.T) {
			st := &adoPRDetailState{status: "active", autoComplete: true, labels: []string{label, mergeReadyLabel}}
			server := newADOMergeQueuePollServer(t, "acme", "proj", "svc", st)
			root, dir := adoMergeQueuePollEnv(t, server.URL, "acme", "proj", "svc", true, map[string]string{
				"pullNumber": "9", "pollIntervalSeconds": "1ms", "pollMaxIntervalSeconds": "2ms", "pollTimeoutSeconds": "5s",
			})
			t.Setenv(executor.InputEnvVar("blockUnansweredFeedback"), "false")
			if code, _, stderr := runArgs(t, "merge-queue-poll", root); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			result := readQueueResult(t, dir)
			st.mu.Lock()
			defer st.mu.Unlock()
			if result["queueOutcome"] != mergeReviewOptOutOutcome || st.cancels != 1 || st.autoComplete {
				t.Fatalf("park not enforced: result=%v cancels=%d armed=%v", result, st.cancels, st.autoComplete)
			}
		})
	}
}
