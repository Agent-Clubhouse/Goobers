package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/providers"
)

// fakeCommitStatus is one POST /statuses/{sha} the fake GitHub server accepted.
type fakeCommitStatus struct {
	sha, context, state, description, targetURL, token string
}

func (s *fakeGitHubServer) handleCommitStatusWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		return
	}
	sha := strings.TrimPrefix(r.URL.Path, "/repos/"+s.owner+"/"+s.repo+"/statuses/")
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad status body", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusWriteFailure != 0 {
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, s.statusWriteFailure)
		return
	}
	if s.commitStatuses == nil {
		s.commitStatuses = map[string][]fakeCommitStatus{}
	}
	s.commitStatuses[sha] = append(s.commitStatuses[sha], fakeCommitStatus{
		sha: sha, context: body["context"], state: body["state"], description: body["description"],
		targetURL: body["target_url"], token: requestActor(r),
	})
	writeFakeJSON(w, map[string]interface{}{"id": len(s.commitStatuses[sha])})
}

// commitStatusesFor renders sha's recorded statuses as the combined-status
// API lists them (oldest first; consumers keep the last per context).
func (s *fakeGitHubServer) commitStatusesFor(sha string) []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]interface{}, 0, len(s.commitStatuses[sha]))
	for _, status := range s.commitStatuses[sha] {
		out = append(out, map[string]interface{}{
			"context": status.context, "state": status.state,
			"description": status.description, "target_url": status.targetURL,
		})
	}
	return out
}

func (s *fakeGitHubServer) postedStatuses() []fakeCommitStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fakeCommitStatus
	for _, statuses := range s.commitStatuses {
		out = append(out, statuses...)
	}
	return out
}

func (s *fakeGitHubServer) seedCommitStatus(sha string, status fakeCommitStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commitStatuses == nil {
		s.commitStatuses = map[string][]fakeCommitStatus{}
	}
	status.sha = sha
	s.commitStatuses[sha] = append(s.commitStatuses[sha], status)
}

// setupMergeReviewStatusRun stages a GitHub merge-review apply-verdict run for
// PR 10, reviewed at (reviewed-head, reviewed-base), whose live PR is at
// (currentHead, currentBase).
func setupMergeReviewStatusRun(t *testing.T, verdict apiv1.Verdict, currentHead, currentBase string) (string, *fakeGitHubServer) {
	t.Helper()
	const runID = "review-run"
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(10, "Selected PR")
	// A real diff, so a non-pass verdict is not closed as moot.
	server.addOpenPR(10, "goobers/implementation/run-10", "main", currentHead, currentBase, false, nil,
		[]fakePRFile{{path: "widget.go", status: "modified", additions: 1, deletions: 1, patch: "@@ -1 +1 @@\n-a\n+b"}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "reviewed-head")
	t.Setenv("GOOBERS_INPUT_SELECTEDBASESHA", "reviewed-base")
	verdict.HeadSHA, verdict.BaseSHA = "reviewed-head", "reviewed-base"
	seedGateVerdictJournal(t, root, runID, verdict)
	t.Chdir(t.TempDir())
	return root, server
}

// TestApplyVerdictPublishesNativeVerdictStatus is the publishing half of the
// #4188 shape (#4199): ordinary CI is green, and merge-review's authoritative
// verdict must reach the provider's merge policy as a commit status on the
// exact reviewed head — failure for needs-changes, success for pass — under
// the review identity, with the reviewed base recorded.
func TestApplyVerdictPublishesNativeVerdictStatus(t *testing.T) {
	tests := []struct {
		decision  apiv1.VerdictDecision
		findings  []apiv1.Finding
		wantState string
	}{
		{decision: apiv1.VerdictNeedsChanges, wantState: "failure", findings: []apiv1.Finding{{
			ID: "f1", Severity: apiv1.SeverityError, Message: "The loop bound is off by one.", Location: "widget.go:12",
		}}},
		{decision: apiv1.VerdictPass, wantState: "success"},
	}
	for _, tt := range tests {
		t.Run(string(tt.decision), func(t *testing.T) {
			root, server := setupMergeReviewStatusRun(t, apiv1.Verdict{
				Decision: tt.decision, Summary: "Reviewed the widget change.", Findings: tt.findings,
			}, "reviewed-head", "reviewed-base")

			code, stdout, stderr := runArgs(t, "apply-verdict", root)
			if code != 0 {
				t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			statuses := server.postedStatuses()
			if len(statuses) != 1 {
				t.Fatalf("posted statuses = %+v, want exactly one verdict status", statuses)
			}
			got := statuses[0]
			if got.sha != "reviewed-head" || got.context != providers.MergeReviewStatusContext || got.state != tt.wantState {
				t.Fatalf("status = %+v, want %s %s on reviewed-head", got, providers.MergeReviewStatusContext, tt.wantState)
			}
			if got.token != "review-token" {
				t.Fatalf("status posted with %q, want the review identity", got.token)
			}
			decision, base, ok := prstatus.ParseVerdictDescription(got.description)
			if !ok || decision != string(tt.decision) || base != "reviewed-base" {
				t.Fatalf("description %q, want %s for base reviewed-base", got.description, tt.decision)
			}
			if !strings.Contains(got.description, "Reviewed the widget change.") || got.targetURL == "" {
				t.Fatalf("status = %+v, want the summary and a link to the verdict", got)
			}

			// merge-pr's poll sees the published verdict apart from CI.
			poll, err := server.newGitHubProvider("t").PollPullRequest(context.Background(), providers.PullRequestPollRequest{
				Repository: providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}, PullID: "10",
			})
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
			refusal := prstatus.VerdictStatusRefusal(poll.VerdictStatus, poll.HeadSHA, "reviewed-base")
			if (tt.decision == apiv1.VerdictPass) != (refusal == "") || poll.CheckState != providers.CheckStatePassing {
				t.Fatalf("poll CI = %s, verdict refusal = %q: want green CI and a refusal only for a non-pass", poll.CheckState, refusal)
			}
		})
	}
}

// TestApplyVerdictWithdrawsStandingVerdictStatusWhenBaseMoved: a pass
// recorded for an older base must not keep authorizing a native merge. When
// the review is void because the base moved under an unchanged head, the
// standing status on that head turns pending; nothing else is published.
func TestApplyVerdictWithdrawsStandingVerdictStatusWhenBaseMoved(t *testing.T) {
	root, server := setupMergeReviewStatusRun(t, apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "ready"},
		"reviewed-head", "moved-base")
	server.seedCommitStatus("reviewed-head", fakeCommitStatus{
		context: providers.MergeReviewStatusContext, state: "success",
		description: prstatus.VerdictDescription(apiv1.VerdictPass, "older-base", "ready"),
	})

	code, stdout, stderr := runArgs(t, "apply-verdict", root)
	if code != 0 || !strings.Contains(stdout, "verdict void") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q; want a voided verdict", code, stdout, stderr)
	}
	statuses := server.postedStatuses()
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want the seeded pass then one withdrawal", statuses)
	}
	withdrawn := statuses[1]
	decision, base, _ := prstatus.ParseVerdictDescription(withdrawn.description)
	if withdrawn.sha != "reviewed-head" || withdrawn.state != "pending" || decision != "stale" || base != "moved-base" {
		t.Fatalf("withdrawal = %+v, want a pending stale status on reviewed-head for moved-base", withdrawn)
	}
}

// TestApplyVerdictNeverPublishesOnAMovedHead: a verdict for an older head never
// touches the new head, which carries no verdict status until reviewed.
func TestApplyVerdictNeverPublishesOnAMovedHead(t *testing.T) {
	root, server := setupMergeReviewStatusRun(t, apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "fix"},
		"pushed-head", "reviewed-base")

	code, stdout, stderr := runArgs(t, "apply-verdict", root)
	if code != 0 || !strings.Contains(stdout, "verdict void") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q; want a voided verdict", code, stdout, stderr)
	}
	if statuses := server.postedStatuses(); len(statuses) != 0 {
		t.Fatalf("statuses = %+v, want none: neither the old nor the new head gets this verdict", statuses)
	}
}

// TestApplyVerdictStatusWriteFailures: a credential that may not write
// statuses degrades to a warning (the verdict comment and label still land),
// while any other status failure fails the stage rather than reporting a
// verdict whose native enforcement is missing.
func TestApplyVerdictStatusWriteFailures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		wantCode int
	}{
		{name: "permission denied degrades", status: http.StatusForbidden, wantCode: 0},
		{name: "server error fails", status: http.StatusBadGateway, wantCode: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, server := setupMergeReviewStatusRun(t, apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "fix", Findings: []apiv1.Finding{{
				ID: "f1", Severity: apiv1.SeverityError, Message: "Broken.", Location: "widget.go:1",
			}}}, "reviewed-head", "reviewed-base")
			server.mu.Lock()
			server.statusWriteFailure = tt.status
			server.mu.Unlock()

			code, stdout, stderr := runArgs(t, "apply-verdict", root)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d; stdout = %q, stderr = %q", code, tt.wantCode, stdout, stderr)
			}
			if tt.wantCode == 0 && !strings.Contains(stderr, "may not write commit statuses") {
				t.Fatalf("stderr = %q, want the missing-grant warning", stderr)
			}
			server.mu.Lock()
			comments := len(server.issues[10].comments)
			server.mu.Unlock()
			if comments != 1 {
				t.Fatalf("verdict comments = %d, want the durable record posted before the status", comments)
			}
		})
	}
}

// TestVerdictRecordPublishesGiteaCommitStatus proves the Gitea arm of the same
// publication: Gitea commit statuses are the merge-policy surface its branch
// protection's status-check patterns gate on.
func TestVerdictRecordPublishesGiteaCommitStatus(t *testing.T) {
	fake := &fakeGiteaComments{t: t, login: "goobers"}
	var status map[string]string
	var statusPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/your-org/your-repo/statuses/", func(w http.ResponseWriter, r *http.Request) {
		statusPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode status: %v", err)
		}
		writeFakeJSON(w, map[string]int{"id": 3})
	})
	mux.HandleFunc("/", fake.serve)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	gitea := providers.NewGiteaProvider(server.URL, "review-token")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "your-org", Name: "your-repo"}
	record := verdictRecord{
		provider: gitea, statusPublisher: gitea, repo: repo, number: 77, author: "goobers",
		current: providers.PullRequestSummary{Number: 77, HeadSHA: "reviewed-head", BaseSHA: "reviewed-base", URL: "https://gitea.test/pr/77"},
	}
	var stdout, stderr bytes.Buffer
	verdict := apiv1.Verdict{Decision: apiv1.VerdictEscalate, Summary: "Needs a human."}
	if err := record.publish(context.Background(), mergeReviewStatusMarker+"\nescalated", verdict, &stdout, &stderr); err != nil {
		t.Fatalf("publish: %v (stderr %q)", err, stderr.String())
	}
	if statusPath != "/api/v1/repos/your-org/your-repo/statuses/reviewed-head" {
		t.Fatalf("status path = %q, want the reviewed head", statusPath)
	}
	if status["context"] != providers.MergeReviewStatusContext || status["state"] != "failure" ||
		!strings.HasPrefix(status["description"], "escalate for base reviewed-base") {
		t.Fatalf("status = %#v, want a failing merge-review status recording the base", status)
	}
	if len(fake.bodies()) != 1 {
		t.Fatalf("comments = %q, want the verdict comment first", fake.bodies())
	}
}

// TestMergePRRefusesWhileVerdictStatusBlocks is the landing half of the #4188
// shape: CI is green and merge-review's verdict input is the shipped
// hardcoded "pass", but the authoritative verdict status on the head is a
// failure, so merge-pr must not land. A pass recorded for an older base does
// not authorize either; the SHA-pinned pass for the pinned base does.
func TestMergePRRefusesWhileVerdictStatusBlocks(t *testing.T) {
	status := func(state string, decision apiv1.VerdictDecision, base string) map[string]interface{} {
		return map[string]interface{}{
			"context": providers.MergeReviewStatusContext, "state": state,
			"description": prstatus.VerdictDescription(decision, base, "review"),
		}
	}
	tests := []struct {
		name       string
		verdict    map[string]interface{}
		wantMerged bool
		wantReason string
	}{
		{name: "needs-changes blocks", verdict: status("failure", apiv1.VerdictNeedsChanges, "base456"), wantReason: "is failure"},
		{name: "older-base pass does not authorize", verdict: status("success", apiv1.VerdictPass, "older-base"), wantReason: "is a pass for base older-base"},
		{name: "pinned pass clears", verdict: status("success", apiv1.VerdictPass, "base456"), wantMerged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &mergePRServerState{checkState: "success", headSHA: "head123", baseSHA: "base456", verdictStatus: tt.verdict}
			server := newMergePRServer(t, "your-org", "your-repo", st)
			root, dir := mergePREnv(t, server.URL, false, map[string]string{
				"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
			})

			code, _, stderr := runArgs(t, "merge-pr", root)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			result := readMergeResult(t, dir)
			merged, _ := result["merged"].(bool)
			if merged != tt.wantMerged || (st.mergeCalls == 1) != tt.wantMerged {
				t.Fatalf("merged = %v (merge calls %d), want %v; result = %+v", merged, st.mergeCalls, tt.wantMerged, result)
			}
			if reason, _ := result["reason"].(string); !strings.Contains(reason, tt.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}
