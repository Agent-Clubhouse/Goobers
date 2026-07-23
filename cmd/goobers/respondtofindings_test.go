package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func seedRemediationResponseRun(t *testing.T, root, runID string, verdict apiv1.Verdict, responses string, published bool) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create remediation run journal: %v", err)
	}
	contextData, err := json.Marshal(apiv1.RemediationBrief{
		Schema: apiv1.RemediationBriefVersion,
		GatherPRContext: apiv1.RemediationPRContext{
			Verdict: &verdict,
		},
	})
	if err != nil {
		t.Fatalf("marshal remediation context: %v", err)
	}
	if _, err := run.RecordArtifact(runID+":gather-pr-context/result", contextData); err != nil {
		t.Fatalf("record pr-context.json: %v", err)
	}
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "implement",
		Attempt: 1,
		Status:  string(apiv1.ResultSuccess),
		Outputs: map[string]any{findingResponsesOutput: responses},
	}); err != nil {
		t.Fatalf("record implement result: %v", err)
	}
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "push-remediated",
		Attempt: 1,
		Status:  string(apiv1.ResultSuccess),
		Outputs: map[string]any{"published": strconv.FormatBool(published)},
	}); err != nil {
		t.Fatalf("record push-remediated result: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close remediation run journal: %v", err)
	}
}

func respondToFindingsFixture(t *testing.T, verdict apiv1.Verdict, responses string, published bool) (string, *fakeGitHubServer, string) {
	t.Helper()
	const (
		runID    = "run-942"
		prNumber = 77
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "Remediated PR")
	previous := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = previous })

	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	resultFile := filepath.Join(t.TempDir(), remediationResponseArtifactName)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	if _, err := claimPullRequest(root, []providers.PullRequestSummary{{Number: prNumber}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	seedRemediationResponseRun(t, root, runID, verdict, responses, published)
	return root, server, resultFile
}

func TestRespondToFindingsPostsCompleteDurableAccount(t *testing.T) {
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{
			{
				Severity: apiv1.SeverityError,
				Class:    apiv1.FindingSubstantive,
				Message:  "validate empty input",
				Location: "internal/parser.go:42",
			},
			{
				Severity: apiv1.SeverityWarning,
				Class:    apiv1.FindingSubstantive,
				Message:  "remove the compatibility fallback",
			},
		},
	}
	responses := `[` +
		`{"finding":2,"disposition":"declined","detail":"The fallback remains required by the documented V0 compatibility contract."},` +
		`{"finding":1,"disposition":"addressed","detail":"Added an explicit empty-input guard and regression coverage."}` +
		`]`
	root, server, resultFile := respondToFindingsFixture(t, verdict, responses, true)

	for attempt := 1; attempt <= 2; attempt++ {
		code, stdout, stderr := runArgs(t, "respond-to-findings", root)
		if code != 0 {
			t.Fatalf("attempt %d: code = %d, stdout = %q, stderr = %q", attempt, code, stdout, stderr)
		}
	}

	server.mu.Lock()
	comments := append([]string(nil), server.issues[77].comments...)
	server.mu.Unlock()
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want one run-scoped response after a retry", len(comments))
	}
	for _, want := range []string{
		"1. **Addressed**",
		"Added an explicit empty-input guard",
		"validate empty input",
		"2. **Declined**",
		"compatibility contract",
		"remove the compatibility fallback",
	} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("comment missing %q:\n%s", want, comments[0])
		}
	}

	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read response result: %v", err)
	}
	var result remediationResponseResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal response result: %v", err)
	}
	if result.SelectedNumber != "77" || result.SourceRunID != "run-942" || result.FindingCount != 2 {
		t.Errorf("result identity/count = %+v, want PR 77, run-942, 2 findings", result)
	}
	if !result.Posted {
		t.Error("result posted = false, want true")
	}
	if len(result.Findings) != 2 || result.Findings[0].Finding != 1 || result.Findings[1].Finding != 2 {
		t.Errorf("result findings = %+v, want complete verdict-order account", result.Findings)
	}
}

func TestRespondToFindingsRejectsIncompleteAccountBeforePosting(t *testing.T) {
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{
			{Severity: apiv1.SeverityError, Message: "first"},
			{Severity: apiv1.SeverityWarning, Message: "second"},
		},
	}
	root, server, _ := respondToFindingsFixture(t, verdict,
		`[{"finding":1,"disposition":"addressed","detail":"fixed the first"}]`, true)

	code, stdout, stderr := runArgs(t, "respond-to-findings", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "want exactly 2") {
		t.Errorf("stderr = %q, want incomplete-account detail", stderr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if got := len(server.issues[77].comments); got != 0 {
		t.Errorf("comments = %d, want none when validation fails", got)
	}
}

func TestRespondToFindingsDoesNotPostWhenPushWasSkipped(t *testing.T) {
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{
			{Severity: apiv1.SeverityError, Message: "fix the parser"},
		},
	}
	root, server, resultFile := respondToFindingsFixture(t, verdict,
		`[{"finding":1,"disposition":"addressed","detail":"added the parser guard"}]`, false)

	code, stdout, stderr := runArgs(t, "respond-to-findings", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "not published") {
		t.Errorf("stdout = %q, want skipped-publication detail", stdout)
	}
	server.mu.Lock()
	comments := len(server.issues[77].comments)
	server.mu.Unlock()
	if comments != 0 {
		t.Errorf("comments = %d, want none for work that was not published", comments)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read response result: %v", err)
	}
	var result remediationResponseResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal response result: %v", err)
	}
	if result.Posted || result.Reason == "" || len(result.Findings) != 1 {
		t.Errorf("result = %+v, want a durable unposted account with its reason", result)
	}
}

func TestValidateFindingResponses(t *testing.T) {
	findings := []apiv1.Finding{
		{Severity: apiv1.SeverityError, Message: "first"},
		{Severity: apiv1.SeverityWarning, Message: "second"},
	}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "missing", raw: "", want: "omitted"},
		{name: "malformed", raw: "{", want: "decode JSON"},
		{name: "duplicate", raw: `[{"finding":1,"disposition":"addressed","detail":"a"},{"finding":1,"disposition":"declined","detail":"b"}]`, want: "more than once"},
		{name: "out of range", raw: `[{"finding":1,"disposition":"addressed","detail":"a"},{"finding":3,"disposition":"declined","detail":"b"}]`, want: "valid range"},
		{name: "bad disposition", raw: `[{"finding":1,"disposition":"addressed","detail":"a"},{"finding":2,"disposition":"skipped","detail":"b"}]`, want: "addressed or declined"},
		{name: "missing detail", raw: `[{"finding":1,"disposition":"addressed","detail":"a"},{"finding":2,"disposition":"declined","detail":" "}]`, want: "no detail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := validateFindingResponses(findings, tt.raw); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateFindingResponses error = %v, want containing %q", err, tt.want)
			}
		})
	}

	responses, err := validateFindingResponses(findings,
		`[{"finding":2,"disposition":"DECLINED","detail":" reason "},{"finding":1,"disposition":"addressed","detail":" change "}]`)
	if err != nil {
		t.Fatalf("valid responses: %v", err)
	}
	if responses[0].Finding != 1 || responses[0].Detail != "change" ||
		responses[1].Finding != 2 || responses[1].Disposition != "declined" {
		t.Errorf("normalized responses = %+v", responses)
	}

	empty, err := validateFindingResponses(nil, "")
	if err != nil || len(empty) != 0 {
		t.Errorf("empty verdict responses = %+v, err = %v; want empty success", empty, err)
	}
}
