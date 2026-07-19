package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestNativeApprovalIsDismissedAndNewHeadIsReviewed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const prNumber = 10
	server.addIssue(prNumber, "Eligible PR")
	server.addOpenPR(prNumber, "goobers/implementation/run-10", "main", "head-one", "base-one", false, nil, nil)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "review-run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	seedGateVerdictJournal(t, root, "review-run-1", apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Summary:  "ready",
		HeadSHA:  "head-one",
		BaseSHA:  "base-one",
	})

	firstReviewDir := t.TempDir()
	t.Chdir(firstReviewDir)
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("first apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	resultData, err := os.ReadFile("verdict-result.json")
	if err != nil {
		t.Fatalf("read verdict-result.json: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(resultData, &result); err != nil {
		t.Fatalf("unmarshal verdict result: %v", err)
	}
	if result["decision"] != "pass" || result["selectedHeadSha"] != "head-one" || result["selectedBaseSha"] != "base-one" {
		t.Fatalf("verdict result = %+v, want a pass pinned to head-one/base-one", result)
	}
	facts := readMutationFacts(t, firstReviewDir)
	if len(facts) != 1 || facts[0].Kind != "pr" || facts[0].ID != "10" || facts[0].Operation != "review" {
		t.Fatalf("mutation facts = %+v, want one native review fact for PR #10", facts)
	}

	server.setPRHead(prNumber, "head-two", nil)
	server.mu.Lock()
	first := server.prs[prNumber].reviews[0]
	issue := server.issues[prNumber]
	server.mu.Unlock()
	if first.state != "DISMISSED" {
		t.Fatalf("approval state after push = %q, want DISMISSED", first.state)
	}
	if len(issue.labels) != 0 {
		t.Fatalf("pass handoff added labels: %v", issue.labels)
	}
	if len(issue.comments) != 1 {
		t.Fatalf("pass verdict comments = %v, want one structured compatibility comment", issue.comments)
	}

	t.Setenv("GOOBERS_RUN_ID", "review-run-2")
	selectDir := t.TempDir()
	t.Chdir(selectDir)
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select after push: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(selectDir, "selected-pr.json"))
	if err != nil {
		t.Fatalf("read selected-pr.json: %v", err)
	}
	var selected map[string]string
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatalf("unmarshal selected PR: %v", err)
	}
	if selected["number"] != "10" || selected["headSha"] != "head-two" {
		t.Fatalf("selected = %+v, want PR #10 at head-two", selected)
	}

	seedGateVerdictJournal(t, root, "review-run-2", apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Summary:  "new head ready",
		HeadSHA:  "head-two",
		BaseSHA:  "base-one",
	})
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", selected["number"])
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("second apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	reviews := append([]fakeReview(nil), server.prs[prNumber].reviews...)
	comments := append([]string(nil), server.issues[prNumber].comments...)
	server.mu.Unlock()
	if len(reviews) != 2 ||
		reviews[0].state != "DISMISSED" ||
		reviews[1].state != "APPROVED" ||
		reviews[1].commitSHA != "head-two" {
		t.Fatalf("reviews = %+v, want dismissed head-one approval followed by approved head-two review", reviews)
	}
	if len(comments) != 1 {
		t.Fatalf("verdict comments = %v, want one sticky status comment", comments)
	}
	posted, ok := parseVerdictComment(comments[0])
	if !ok || posted.HeadSHA != "head-two" || posted.Summary != "new head ready" {
		t.Fatalf("sticky verdict = %+v, ok = %v, want updated head-two verdict", posted, ok)
	}
}
