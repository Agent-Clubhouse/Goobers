package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// #6134: merge-review must not land a PR over open human review feedback
// newer than Goobers' last response, whatever the forge enforces.

func feedbackAt(minutes int) *time.Time {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)
	return &at
}

// fakeMergeFeedbackReader is a mergeFeedbackReader over fixed feedback.
type fakeMergeFeedbackReader struct {
	general []providers.Comment
	threads []feedbackThread
	self    commentIdentity
	err     error
}

func (f fakeMergeFeedbackReader) feedback(context.Context, string) ([]providers.Comment, []feedbackThread, error) {
	return f.general, f.threads, f.err
}

func (f fakeMergeFeedbackReader) identity(context.Context) (commentIdentity, error) {
	return f.self, nil
}

func TestMergeFeedbackGateDecisions(t *testing.T) {
	self := loginCommentIdentity("goobers-bot")
	ask := providers.Comment{Author: "alice", Body: "Please add a README note.", CreatedAt: feedbackAt(10), URL: "https://forge/pr/9#c1"}
	answer := providers.Comment{
		Author: "goobers-bot", CreatedAt: feedbackAt(20),
		Body: stampOwnFixtureBody(withFeedbackAck("Added the note.", feedbackAckMarker(*feedbackAt(10))), "comment"),
	}
	verdictAfter := providers.Comment{
		Author: "goobers-bot", CreatedAt: feedbackAt(30),
		Body: stampOwnFixtureBody(renderVerdictComment(passVerdictForFeedbackTests()), "comment"),
	}
	tests := []struct {
		name    string
		general []providers.Comment
		threads []feedbackThread
		mode    string
		blocked bool
	}{
		{name: "no feedback lands"},
		{name: "unanswered general comment blocks", general: []providers.Comment{ask}, blocked: true},
		{name: "answered general comment lands", general: []providers.Comment{ask, answer}},
		{name: "a later verdict does not answer a person", general: []providers.Comment{ask, verdictAfter}, blocked: true},
		{name: "active thread newer than the last Goobers reply blocks", threads: []feedbackThread{{comments: []providers.Comment{answer, {Author: "alice", Body: "still wrong", CreatedAt: feedbackAt(40)}}}}, blocked: true},
		{name: "resolved thread lands", threads: []feedbackThread{{resolved: true, comments: []providers.Comment{ask}}}},
		{name: "a Goobers answer in a resolved thread still acknowledges", general: []providers.Comment{ask}, threads: []feedbackThread{{resolved: true, comments: []providers.Comment{answer}}}},
		{name: "a bot reviewer is automation", general: []providers.Comment{{Author: "linter[bot]", AuthorType: "Bot", Body: "nit", CreatedAt: feedbackAt(5)}}},
		{name: "shared identity: unmarked own-login comment blocks", general: []providers.Comment{{Author: "goobers-bot", Body: "please fix", CreatedAt: feedbackAt(5)}}, mode: commentIdentityShared, blocked: true},
		{name: "dedicated identity: unmarked own-login comment lands", general: []providers.Comment{{Author: "goobers-bot", Body: "please fix", CreatedAt: feedbackAt(5)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode := tt.mode
			if mode == "" {
				mode = commentIdentityDedicated
			}
			gate := mergeFeedbackGate{
				settings: commentIdentitySettings{mode: mode},
				reader:   fakeMergeFeedbackReader{general: tt.general, threads: tt.threads, self: self},
			}
			reasons := gate.refuse(context.Background(), "9", nil)
			if blocked := len(reasons) == 1 && strings.HasPrefix(reasons[0], unansweredHumanFeedbackReasonCode+":"); blocked != tt.blocked {
				t.Fatalf("reasons = %q, want blocked=%v", reasons, tt.blocked)
			}
		})
	}
}

func TestMergeFeedbackGateFailsClosedAndOptsOut(t *testing.T) {
	ask := []providers.Comment{{Author: "alice", Body: "Please add a README note.", CreatedAt: feedbackAt(10)}}
	unreadable := mergeFeedbackGate{reader: fakeMergeFeedbackReader{err: errors.New("review threads unsupported")}}
	if reasons := unreadable.refuse(context.Background(), "9", nil); len(reasons) != 1 || !strings.HasPrefix(reasons[0], humanFeedbackUnverifiableReasonCode+":") {
		t.Fatalf("unreadable feedback reasons = %q, want one %s refusal", reasons, humanFeedbackUnverifiableReasonCode)
	}
	disabled := mergeFeedbackGate{disabled: true, reader: fakeMergeFeedbackReader{general: ask}}
	if reasons := disabled.refuse(context.Background(), "9", nil); len(reasons) != 0 {
		t.Fatalf("opted-out gate reasons = %q, want none", reasons)
	}
	other := mergeFeedbackGate{reader: fakeMergeFeedbackReader{general: ask}}
	if reasons := other.refuse(context.Background(), "9", []string{"CI is failing"}); len(reasons) != 1 {
		t.Fatalf("reasons = %q, want only the earlier conjunct (no feedback read once refused)", reasons)
	}

	t.Setenv("GOOBERS_INPUT_BLOCKUNANSWEREDFEEDBACK", "sometimes")
	invalid := newMergeFeedbackGate(providers.RepositoryRef{Provider: providers.ProviderGitHub}, nil, "goobers-bot", false)
	if reasons := invalid.refuse(context.Background(), "9", nil); len(reasons) != 1 || !strings.Contains(reasons[0], "blockUnansweredFeedback") {
		t.Fatalf("invalid setting reasons = %q, want a fail-closed refusal naming it", reasons)
	}
	t.Setenv("GOOBERS_INPUT_BLOCKUNANSWEREDFEEDBACK", "true")
	noThreads := newMergeFeedbackGate(providers.RepositoryRef{Provider: providers.ProviderGitHub}, nil, "goobers-bot", false)
	if reasons := noThreads.refuse(context.Background(), "9", nil); len(reasons) != 1 || !strings.HasPrefix(reasons[0], humanFeedbackUnverifiableReasonCode+":") {
		t.Fatalf("provider without a thread read reasons = %q, want a fail-closed refusal", reasons)
	}
}

func TestReviewThreadFeedbackMapping(t *testing.T) {
	listing := providers.PullRequestReviewThreads{
		Reviews: []providers.PullRequestNativeReview{
			{ID: 1, Author: "alice", State: "CHANGES_REQUESTED", Body: "rename X", SubmittedAt: feedbackAt(1)},
			{ID: 2, Author: "alice", State: "APPROVED", Body: ""},
			{ID: 3, Author: "bob", State: "DISMISSED", Body: "old ask", SubmittedAt: feedbackAt(2)},
			{ID: 4, Author: "carol", State: "PENDING", Body: "draft", SubmittedAt: feedbackAt(3)},
			{ID: 5, Author: "reviewer[bot]", State: "COMMENTED", Body: "lint", SubmittedAt: feedbackAt(4)},
			{ID: 6, Author: "bob", State: "APPROVED", Body: "LGTM, ship it", SubmittedAt: feedbackAt(4)},
		},
		InlineComments: []providers.PullRequestInlineComment{
			{ID: 10, ThreadID: "T1", Author: "alice", Body: "a", CreatedAt: feedbackAt(5)},
			{ID: 11, ThreadID: "T2", Author: "bob", Body: "b", IsResolved: true, CreatedAt: feedbackAt(6)},
			{ID: 12, ThreadID: "T1", Author: "goobers-bot", Body: "c", CreatedAt: feedbackAt(7)},
			{ID: 13, ThreadID: "T3", Author: "Copilot", AuthorType: "Bot", Body: "consider X", CreatedAt: feedbackAt(8)},
		},
	}
	general, threads := reviewThreadFeedback(listing)
	if len(general) != 2 || general[0].Body != "rename X" || general[1].AuthorType != "Bot" {
		t.Fatalf("general = %+v, want the change request and the bot review only", general)
	}
	if len(threads) != 3 || len(threads[0].comments) != 2 || threads[0].resolved || !threads[1].resolved || threads[2].comments[0].AuthorType != "Bot" {
		t.Fatalf("threads = %+v, want T1 (open, 2 comments) and T2 (resolved)", threads)
	}
	open := openFeedbackComments(general, threads, newCommentClassifier(loginCommentIdentity("goobers-bot"), commentIdentitySettings{mode: commentIdentityDedicated}))
	var bodies []string
	for _, c := range open {
		bodies = append(bodies, c.Body)
	}
	if strings.Join(bodies, ",") != "rename X,lint,a,c,consider X" {
		t.Fatalf("open feedback = %v, want time-ordered and the resolved human comment dropped", bodies)
	}
}

// GitHub end to end: merge-pr refuses over an unanswered inline review thread
// and lands once it is resolved, or when the gate is opted out.
func TestMergePRUnansweredReviewThreadGitHub(t *testing.T) {
	inline := []map[string]interface{}{{
		"id": 501, "body": "Please add a short note to the README describing this change.", "path": "a.go",
		"created_at": "2026-10-01T12:00:00Z", "user": map[string]string{"login": "alice"},
	}}
	thread := func(resolved bool) []map[string]interface{} {
		return []map[string]interface{}{{
			"id": "THREAD_1", "isResolved": resolved, "path": "a.go",
			"comments": map[string]interface{}{"nodes": []map[string]interface{}{{"databaseId": 501}}},
		}}
	}
	tests := []struct {
		name       string
		resolved   bool
		optOut     bool
		failThread bool
		wantMerged bool
		wantReason string
	}{
		{name: "active human thread blocks", wantReason: unansweredHumanFeedbackReasonCode},
		{name: "resolved thread lands", resolved: true, wantMerged: true},
		{name: "opt-out lands", optOut: true, wantMerged: true},
		{name: "unreadable threads fail closed", failThread: true, wantReason: humanFeedbackUnverifiableReasonCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &mergePRServerState{
				checkState: "success", headSHA: "head123", baseSHA: "base456",
				inlineComments: inline, reviewThreads: thread(tt.resolved),
			}
			if tt.failThread {
				st.reviewThreadsStatus = 500
			}
			server := newMergePRServer(t, "your-org", "your-repo", st)
			inputs := map[string]string{
				"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456", "verdictAuthor": "goobers",
			}
			if tt.optOut {
				inputs["blockUnansweredFeedback"] = "false"
			}
			root, dir := mergePREnv(t, server.URL, false, inputs)
			code, stdout, stderr := runArgs(t, "merge-pr", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			result := readMergeResult(t, dir)
			merged, _ := result["merged"].(bool)
			if merged != tt.wantMerged {
				t.Fatalf("result = %+v, want merged=%v", result, tt.wantMerged)
			}
			if tt.wantMerged {
				return
			}
			if st.mergeCalls != 0 {
				t.Fatalf("merge endpoint called %d times over unanswered feedback", st.mergeCalls)
			}
			if reason, _ := result["reason"].(string); !strings.HasPrefix(reason, tt.wantReason+":") {
				t.Fatalf("reason = %q, want prefix %s", reason, tt.wantReason)
			}
		})
	}
}

// A plain PR comment left after the pass verdict blocks on GitHub too: the
// verdict is bookkeeping, not an answer.
func TestMergePRUnansweredGeneralCommentGitHub(t *testing.T) {
	st := &mergePRServerState{
		checkState: "success", headSHA: "head123", baseSHA: "base456",
		extraComments: []map[string]interface{}{{
			"id": 2, "body": "Hold on — please add a README note first.", "created_at": "2026-10-01T12:00:00Z",
			"user": map[string]string{"login": "alice"},
		}},
	}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456", "verdictAuthor": "goobers",
	})
	if code, stdout, stderr := runArgs(t, "merge-pr", root); code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readMergeResult(t, dir)
	if merged, _ := result["merged"].(bool); merged || st.mergeCalls != 0 {
		t.Fatalf("result = %+v (merge calls %d), want a refusal over the unanswered comment", result, st.mergeCalls)
	}
	if reason, _ := result["reason"].(string); !strings.Contains(reason, "alice") {
		t.Fatalf("reason = %q, want the commenter named", reason)
	}
}

// adoFeedbackThread is a raw ADO thread holding one person's comment.
func adoFeedbackThreadFixture(id int, status, authorID, published string) map[string]any {
	return map[string]any{
		"id": id, "status": status,
		"comments": []map[string]any{{
			"id": 1, "commentType": "text", "content": "Please add a short note to the README describing this change.",
			"author":        map[string]string{"id": authorID, "displayName": "A Reviewer"},
			"publishedDate": published,
		}},
	}
}

// Azure DevOps end to end: an active human thread newer than Goobers' last
// response blocks — including one written through Goobers' own identity
// under the default shared mode; a resolved one, or the opt-out, lands.
func TestMergePRUnansweredThreadADO(t *testing.T) {
	tests := []struct {
		name       string
		thread     map[string]any
		optOut     bool
		wantMerged bool
	}{
		{name: "active human thread blocks", thread: adoFeedbackThreadFixture(21, "active", "reviewer-guid", "2026-10-01T12:00:00Z")},
		{name: "shared identity: own-identity active thread blocks", thread: adoFeedbackThreadFixture(21, "active", adoMergePRSelfID, "2026-10-01T12:00:00Z")},
		{name: "resolved thread lands", thread: adoFeedbackThreadFixture(21, "fixed", "reviewer-guid", "2026-10-01T12:00:00Z"), wantMerged: true},
		{name: "opt-out lands", thread: adoFeedbackThreadFixture(21, "active", "reviewer-guid", "2026-10-01T12:00:00Z"), optOut: true, wantMerged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, state := newADOMergePRServer(t, "headsha1", "basesha1")
			state.feedbackThreads.Store([]map[string]any{tt.thread})
			inputs := map[string]string{"pullNumber": "359", "verdict": "pass", "headSha": "headsha1", "baseSha": "basesha1"}
			if tt.optOut {
				inputs["blockUnansweredFeedback"] = "false"
			}
			root, dir := adoMergePREnv(t, server.URL, false, inputs)
			code, stdout, stderr := runArgs(t, "merge-pr", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			result := readMergeResult(t, dir)
			if merged, _ := result["merged"].(bool); merged != tt.wantMerged {
				t.Fatalf("result = %+v, want merged=%v", result, tt.wantMerged)
			}
			if tt.wantMerged {
				return
			}
			if calls := atomic.LoadInt64(&state.patchCalls); calls != 0 {
				t.Fatalf("completion PATCH called %d times over unanswered feedback", calls)
			}
			if reason, _ := result["reason"].(string); !strings.HasPrefix(reason, unansweredHumanFeedbackReasonCode+":") {
				t.Fatalf("reason = %q, want prefix %s", reason, unansweredHumanFeedbackReasonCode)
			}
		})
	}
}

func passVerdictForFeedbackTests() apiv1.Verdict {
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "ready"}
}
