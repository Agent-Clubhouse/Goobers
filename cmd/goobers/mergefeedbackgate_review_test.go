package main

import (
	"strings"
	"sync/atomic"
	"testing"
)

// A code-review bot whose login carries no "[bot]" suffix (GitHub reports its
// account type as Bot) is automation: its unresolved thread does not block.
func TestMergePRBotReviewThreadLandsGitHub(t *testing.T) {
	st := &mergePRServerState{
		checkState: "success", headSHA: "head123", baseSHA: "base456",
		inlineComments: []map[string]interface{}{{
			"id": 502, "body": "Consider extracting this helper.", "path": "a.go",
			"created_at": "2026-10-01T12:00:00Z", "user": map[string]string{"login": "Copilot", "type": "Bot"},
		}},
		reviewThreads: []map[string]interface{}{{
			"id": "THREAD_2", "isResolved": false, "path": "a.go",
			"comments": map[string]interface{}{"nodes": []map[string]interface{}{{"databaseId": 502}}},
		}},
	}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456", "verdictAuthor": "goobers",
	})
	if code, stdout, stderr := runArgs(t, "merge-pr", root); code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if result := readMergeResult(t, dir); result["merged"] != true {
		t.Fatalf("result = %+v, want a bot-typed reviewer's thread not to block the merge", result)
	}
}

// Azure DevOps fails closed: a thread comment the gate cannot attribute (no
// author id) refuses as human-feedback-unverifiable and never completes.
func TestMergePRUnreadableThreadFailsClosedADO(t *testing.T) {
	server, state := newADOMergePRServer(t, "headsha1", "basesha1")
	state.feedbackThreads.Store([]map[string]any{adoFeedbackThreadFixture(21, "active", "", "2026-10-01T12:00:00Z")})
	root, dir := adoMergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "359", "verdict": "pass", "headSha": "headsha1", "baseSha": "basesha1",
	})
	if code, stdout, stderr := runArgs(t, "merge-pr", root); code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readMergeResult(t, dir)
	reason, _ := result["reason"].(string)
	if result["merged"] != false || !strings.HasPrefix(reason, humanFeedbackUnverifiableReasonCode+":") {
		t.Fatalf("result = %+v, want a %s refusal", result, humanFeedbackUnverifiableReasonCode)
	}
	if calls := atomic.LoadInt64(&state.patchCalls); calls != 0 {
		t.Fatalf("completion PATCH called %d times over unverifiable feedback", calls)
	}
}
