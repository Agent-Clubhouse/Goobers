package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestApplyVerdictDegradesOnSelfReview402 is #870's acceptance: on a
// single-GitHub-identity instance the reviewing token is also the PR author,
// so GitHub 422s the native Review ("Can not approve your own pull request").
// That 422 must NOT fail the stage — the native Review is not a merge
// prerequisite (merge-pr reads the verdict from the comment/label handoff, not
// a platform Review). apply-verdict must degrade: skip the native Review, still
// post the verdict comment, and exit 0 so the PR stays on the automated merge
// path. Before the fix this failed the stage in ~3.4s, blocking every
// daemon-authored PR from ever merging.
func TestApplyVerdictDegradesOnSelfReview422(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	const selectedNumber = 10
	server.addIssue(selectedNumber, "Selected PR")
	server.addOpenPR(selectedNumber, "goobers/implementation/run-10", "main", "sha10head", "shamainbase",
		false, nil, nil)
	// The reviewing identity authored this PR — GitHub refuses a self-review.
	server.setPRSelfReview(selectedNumber)

	const runID = "run-1"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")

	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Summary:  "looks good",
		HeadSHA:  "sha10head",
		BaseSHA:  "shamainbase",
	})

	applyDir := t.TempDir()
	t.Chdir(applyDir)
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")

	code, stdout, stderr := runArgs(t, "apply-verdict", root)
	if code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "native review skipped") {
		t.Fatalf("stdout = %q, want it to report the native review was skipped", stdout)
	}

	server.mu.Lock()
	issue := server.issues[selectedNumber]
	reviews := append([]fakeReview(nil), server.prs[selectedNumber].reviews...)
	server.mu.Unlock()

	if len(reviews) != 0 {
		t.Fatalf("native reviews = %+v, want none (self-review was refused and skipped)", reviews)
	}
	if issue == nil {
		t.Fatal("selected PR's issue record vanished")
	}
	// The pass path still posts the verdict comment — the handoff merge-pr
	// actually consumes.
	if len(issue.comments) != 1 {
		t.Fatalf("comments = %v, want exactly the one verdict comment", issue.comments)
	}
	if !strings.Contains(issue.comments[0], "pass") {
		t.Fatalf("verdict comment = %q, want it to carry the pass verdict", issue.comments[0])
	}

	// The result file must record the pass decision so published-verdict/merge-pr
	// proceed exactly as they would have on a successful native Review.
	resData, err := os.ReadFile(filepath.Join(applyDir, "verdict-result.json"))
	if err != nil {
		t.Fatalf("read verdict-result.json: %v", err)
	}
	if !strings.Contains(string(resData), `"decision":"pass"`) {
		t.Fatalf("verdict-result.json = %q, want decision pass", resData)
	}
}
