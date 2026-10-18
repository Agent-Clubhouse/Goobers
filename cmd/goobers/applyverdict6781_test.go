package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestPassingMergeReviewDismissesSupersededOwnChangeRequest(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const prNumber = 10
	server.addIssue(prNumber, "Reviewed PR")
	server.addOpenPR(prNumber, "goobers/implementation/run-10", "main", "head-one", "base-one", false, nil, []fakePRFile{{
		path: "fix.go", status: "modified", additions: 1,
	}})
	server.addPRReviewAs(prNumber, "human-reviewer", "CHANGES_REQUESTED")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "review-run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	seedGateVerdictJournal(t, root, "review-run-1", apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "needs a fix",
		HeadSHA:  "head-one",
		BaseSHA:  "base-one",
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "fix the defect",
		}},
	})
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("first apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.setPRHead(prNumber, "head-two", nil)
	t.Setenv("GOOBERS_RUN_ID", "review-run-2")
	seedGateVerdictJournal(t, root, "review-run-2", apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Summary:  "resolved",
		HeadSHA:  "head-two",
		BaseSHA:  "base-one",
	})
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("second apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	reviews := append([]fakeReview(nil), server.prs[prNumber].reviews...)
	server.mu.Unlock()
	if len(reviews) != 3 {
		t.Fatalf("reviews = %+v, want human request, dismissed Goobers request, and approval", reviews)
	}
	if reviews[0].state != "CHANGES_REQUESTED" || reviews[0].author != "human-reviewer" {
		t.Fatalf("human review = %+v, want untouched CHANGES_REQUESTED", reviews[0])
	}
	if reviews[1].state != "DISMISSED" || reviews[1].author != server.authenticatedLogin {
		t.Fatalf("prior Goobers review = %+v, want dismissed", reviews[1])
	}
	if reviews[2].state != "APPROVED" || reviews[2].commitSHA != "head-two" {
		t.Fatalf("latest Goobers review = %+v, want head-two approval", reviews[2])
	}
}
