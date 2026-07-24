package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func seedRemediationBriefRun(t *testing.T, root, runID string, brief apiv1.RemediationBrief) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create remediation run journal: %v", err)
	}
	data, err := json.Marshal(brief)
	if err != nil {
		t.Fatalf("marshal remediation brief: %v", err)
	}
	if _, err := run.RecordArtifact(runID+":gather-pr-context/result", data); err != nil {
		t.Fatalf("record remediation brief: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close remediation run journal: %v", err)
	}
}

func issueContextBrief() apiv1.RemediationBrief {
	return apiv1.RemediationBrief{
		Schema:                 apiv1.RemediationBriefVersion,
		SelectedNumber:         "77",
		Head:                   "goobers/implementation/run-77",
		Base:                   "main",
		WorkspaceBranch:        "goobers/implementation/run-77",
		IsBehindBase:           true,
		HasSubstantiveFindings: "true",
		HasFailingCI:           "false",
		GatherPRContext: apiv1.RemediationPRContext{
			HeadSHA: "head-sha",
			BaseSHA: "base-sha",
			Comments: []apiv1.RemediationThreadComment{
				{Author: "reviewer", Body: "Keep this context."},
			},
		},
		GatherCIFailures: &apiv1.RemediationCIFailures{
			Checks: []apiv1.RemediationCIFailure{},
		},
	}
}

func TestGatherIssueContextAddsResolvableClosingIssuesAndPreservesBrief(t *testing.T) {
	const runID = "run-945"
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(77, "goobers/implementation/run-77", "main", "head-sha", "base-sha", false, nil, nil)
	server.addIssue(945, "Originating issue")
	server.setPRBody(77, "Implements #111\n\nFixes #945")
	server.issues[945].body = "## Acceptance criteria\n\n- Include this body."

	original := issueContextBrief()
	seedRemediationBriefRun(t, root, runID, original)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	dir := t.TempDir()
	t.Chdir(dir)

	if code, stdout, stderr := runArgs(t, "gather-issue-context", root); code != 0 {
		t.Fatalf("gather-issue-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read remediation brief: %v", err)
	}
	var got apiv1.RemediationBrief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal remediation brief: %v", err)
	}

	want := original
	want.GatherIssueContext = &apiv1.RemediationIssueContext{
		Issues: []apiv1.RemediationIssue{{
			Number: "945",
			Title:  "Originating issue",
			Body:   "## Acceptance criteria\n\n- Include this body.",
			URL:    "https://example/issues/945",
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remediation brief = %#v, want %#v", got, want)
	}
}

func TestGatherIssueContextWithUnresolvableClosingIssueEmitsEmptySection(t *testing.T) {
	const runID = "run-945-empty"
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(77, "goobers/implementation/run-77", "main", "head-sha", "base-sha", false, nil, nil)
	server.setPRBody(77, "Fixes #945")

	seedRemediationBriefRun(t, root, runID, issueContextBrief())
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	dir := t.TempDir()
	t.Chdir(dir)

	if code, stdout, stderr := runArgs(t, "gather-issue-context", root); code != 0 {
		t.Fatalf("gather-issue-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read remediation brief: %v", err)
	}
	var got apiv1.RemediationBrief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal remediation brief: %v", err)
	}
	if got.GatherIssueContext == nil {
		t.Fatal("gatherIssueContext is nil, want an explicit checked-and-empty section")
	}
	if len(got.GatherIssueContext.Issues) != 0 {
		t.Fatalf("gatherIssueContext.issues = %v, want empty", got.GatherIssueContext.Issues)
	}
}

// TestGatherIssueContextUsesIssuesCredentialForIssueReads pins the capability
// split: PR listing authenticates with github:pr:write and originating-issue
// reads with github:issues:write, which per-capability credential overrides may
// back with different tokens. The PR is served by a server that does not know
// the issue, and the issue only by a second server reachable with the issues
// token; if GetWorkItem authenticated with the PR credential (the prior bug) it
// would 404 the issue and drop it from the context.
func TestGatherIssueContextUsesIssuesCredentialForIssueReads(t *testing.T) {
	const runID = "run-945-creds"
	const prToken, issuesToken = "pr-token", "issues-token"
	root := initDemo(t)

	prServer := newFakeGitHubServer(t, "your-org", "your-repo")
	prServer.addOpenPR(77, "goobers/implementation/run-77", "main", "head-sha", "base-sha", false, nil, nil)
	prServer.setPRBody(77, "Fixes #945")

	issuesServer := newFakeGitHubServer(t, "your-org", "your-repo")
	issuesServer.addIssue(945, "Originating issue")
	issuesServer.issues[945].body = "issue-body-from-issues-credential"

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		if token == issuesToken {
			return issuesServer.newGitHubProvider(token, opts...)
		}
		return prServer.newGitHubProvider(token, opts...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	seedRemediationBriefRun(t, root, runID, issueContextBrief())
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", prToken)
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", issuesToken)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	dir := t.TempDir()
	t.Chdir(dir)

	if code, stdout, stderr := runArgs(t, "gather-issue-context", root); code != 0 {
		t.Fatalf("gather-issue-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read remediation brief: %v", err)
	}
	var got apiv1.RemediationBrief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal remediation brief: %v", err)
	}
	if got.GatherIssueContext == nil || len(got.GatherIssueContext.Issues) != 1 {
		t.Fatalf("expected issue #945 resolved via the issues credential, got %#v", got.GatherIssueContext)
	}
	if body := got.GatherIssueContext.Issues[0].Body; body != "issue-body-from-issues-credential" {
		t.Fatalf("issue body = %q, want it fetched from the issues-credential server", body)
	}
}
