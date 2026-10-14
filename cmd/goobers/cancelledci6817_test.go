package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// outageCheckRun is a check run a hosted-runner outage cancelled before any
// step ran.
func outageCheckRun(id int, name, conclusion string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "name": name, "status": "completed", "conclusion": conclusion,
		"app": map[string]interface{}{"id": 15368},
	}
}

// TestRemediationTierIgnoresOutageCancelledChecks covers #6817 on the read
// pr-remediation selects by: GitHub's rollup reports FAILURE for a head whose
// every check was cancelled, and that head must not land in the failing-CI
// tier. A real failure beside the cancellations still does.
func TestRemediationTierIgnoresOutageCancelledChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		runs []map[string]interface{}
		want remediationPriority
	}{
		"outage": {
			runs: []map[string]interface{}{
				outageCheckRun(1, "build", "cancelled"),
				outageCheckRun(2, "test", "cancelled"),
				outageCheckRun(3, "setup", "startup_failure"),
			},
			want: remediationPriorityNone,
		},
		"real failure beside cancellations": {
			runs: []map[string]interface{}{
				outageCheckRun(1, "build", "cancelled"),
				outageCheckRun(2, "test", "failure"),
			},
			want: remediationPriorityFailingCI,
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := gatherPRContextServer{
				owner: "your-org", repo: "your-repo", prNumber: 60,
				head: "goobers/impl/run-outage", base: "main",
				headSHA: "outage-head", baseSHA: "base-sha", checkRuns: tc.runs,
			}
			server := srv.start(t)
			provider := mergePRTestServer{url: server.URL}.newGitHubProvider("test-token")
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
			prs := []providers.PullRequestSummary{{Number: 60, HeadSHA: "outage-head"}}
			if err := resolveRemediationCheckStates(context.Background(), provider, repo, prs); err != nil {
				t.Fatalf("resolveRemediationCheckStates: %v", err)
			}
			if got := remediationPriorityFor(prs[0]); got != tc.want {
				t.Fatalf("remediation tier = %v (check state %q), want %v", got, prs[0].CheckState, tc.want)
			}
		})
	}
}

// TestGatherPRContextDoesNotRemediateOutageCancelledCI is #6817's end-to-end
// acceptance for pr-remediation: during a CI-provider outage every check on
// the PR head was cancelled with zero steps run. gather-pr-context must find
// no failing-CI work (no agent session, no remediation or escalation budget
// spent) and must not label or comment on the PR (no park).
func TestGatherPRContextDoesNotRemediateOutageCancelledCI(t *testing.T) {
	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 61, head: "goobers/impl/run-outage", base: "main",
		headSHA: "outage-head", baseSHA: "base-sha",
		checkRuns: []map[string]interface{}{
			outageCheckRun(1, "build", "cancelled"),
			outageCheckRun(2, "test", "cancelled"),
			outageCheckRun(3, "lint", "cancelled"),
		},
		// A sibling keeps the lone-PR behind-base fallback out of the way,
		// so only the CI tier is under test.
		includeUnselected: true,
	}
	server := srv.start(t)

	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-outage")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("gather-pr-context code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") || stderr != "" {
		t.Fatalf("stdout = %q, stderr = %q, want no work for an outage-cancelled head", stdout, stderr)
	}
	assertNoWorkProviderStageResult(t, filepath.Join(workDir, remediationBriefResultFile))
	if len(srv.labels) != 0 || len(srv.comments) != 0 {
		t.Fatalf("labels = %v, comments = %v; an outage must not park the PR", srv.labels, srv.comments)
	}
}
