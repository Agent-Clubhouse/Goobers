package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func seedRemediationResponseRun(t *testing.T, root, runID string, verdict apiv1.Verdict, responses string, published bool) {
	t.Helper()
	seedRemediationResponseRunState(t, root, runID, &verdict, responses, &published)
}

func seedRemediationResponseRunBeforePush(t *testing.T, root, runID string, verdict apiv1.Verdict, responses string) {
	t.Helper()
	seedRemediationResponseRunState(t, root, runID, &verdict, responses, nil)
}

// seedRemediationResponseRunState seeds a nil verdict when verdict is nil,
// the shape produced by remediation causes that carry no merge review
// (failing-ci, sibling-overlap).
func seedRemediationResponseRunState(t *testing.T, root, runID string, verdict *apiv1.Verdict, responses string, published *bool) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create remediation run journal: %v", err)
	}
	contextData, err := json.Marshal(apiv1.RemediationBrief{
		Schema:    apiv1.RemediationBriefVersion,
		Integrity: apiv1.IntegrityUnapproved,
		GatherPRContext: apiv1.RemediationPRContext{
			Verdict: verdict,
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
	if published != nil {
		if err := run.Append(journal.Event{
			Type:    journal.EventStageFinished,
			Stage:   "push-remediated",
			Attempt: 1,
			Status:  string(apiv1.ResultSuccess),
			Outputs: map[string]any{"published": strconv.FormatBool(*published)},
		}); err != nil {
			t.Fatalf("record push-remediated result: %v", err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close remediation run journal: %v", err)
	}
}

func respondToFindingsFixture(t *testing.T, verdict apiv1.Verdict, responses string, published bool) (string, *fakeGitHubServer, string) {
	t.Helper()
	return respondToFindingsFixtureForVerdict(t, &verdict, responses, published)
}

func respondToFindingsFixtureForVerdict(t *testing.T, verdict *apiv1.Verdict, responses string, published bool) (string, *fakeGitHubServer, string) {
	t.Helper()
	t.Chdir(t.TempDir())
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
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	resultFile := filepath.Join(t.TempDir(), remediationResponseArtifactName)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	if _, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{{Number: prNumber}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	seedRemediationResponseRunState(t, root, runID, verdict, responses, &published)
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

func TestRespondToFindingsDispatchesToGitea(t *testing.T) {
	t.Chdir(t.TempDir())
	const (
		runID    = "run-gitea-response"
		prNumber = 77
		token    = "gitea-issues-token"
	)
	var comments []map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token "+token {
			t.Errorf("Authorization = %q, want Gitea token", got)
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			writeFakeJSON(w, map[string]string{"login": "remediation-bot"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/your-org/your-repo/issues/77/comments":
			writeFakeJSON(w, comments)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/your-org/your-repo/issues/77":
			writeFakeJSON(w, map[string]interface{}{
				"id": 77, "number": prNumber, "title": "Remediated PR", "state": "open",
				"html_url": "https://gitea.test/your-org/your-repo/pulls/77",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/your-org/your-repo/issues/77/comments":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode Gitea comment: %v", err)
			}
			comment := map[string]interface{}{
				"id": 1, "body": request["body"], "user": map[string]string{"login": "remediation-bot"},
			}
			comments = append(comments, comment)
			writeFakeJSON(w, comment)
		default:
			t.Errorf("unexpected Gitea request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	root := initDemo(t)
	configureRemediationGitea(t, root, server.URL)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitea))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", token)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), remediationResponseArtifactName))
	if _, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{{Number: prNumber}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	seedRemediationResponseRun(t, root, runID, apiv1.Verdict{}, "", true)

	code, stdout, stderr := runArgs(t, "respond-to-findings", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if len(comments) != 1 || !strings.Contains(comments[0]["body"].(string), remediationResponseMarker(runID)) {
		t.Fatalf("Gitea comments = %+v, want one run-scoped remediation response", comments)
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
	root, server, resultFile := respondToFindingsFixture(t, verdict,
		`[{"finding":1,"disposition":"addressed","detail":"fixed the first"}]`, true)

	code, stdout, stderr := runArgs(t, "respond-to-findings", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "verdict finding 2 (second) has no response") {
		t.Errorf("stderr = %q, want the unanswered verdict finding named", stderr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if got := len(server.issues[77].comments); got != 0 {
		t.Errorf("comments = %d, want none when validation fails", got)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read validation failure result: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal validation failure result: %v", err)
	}
	if got := result["errorCode"]; got != errorCodeFindingResponsesInvalid {
		t.Errorf("errorCode = %v, want %q", got, errorCodeFindingResponsesInvalid)
	}
}

func TestRespondToFindingsCheckValidatesBeforePush(t *testing.T) {
	const runID = "run-check"
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{
			{Severity: apiv1.SeverityError, Message: "first"},
			{Severity: apiv1.SeverityWarning, Message: "second"},
		},
	}
	tests := []struct {
		name      string
		responses string
		wantCode  int
		wantText  string
	}{
		{
			name:      "complete",
			responses: `[{"finding":1,"disposition":"addressed","detail":"fixed first"},{"finding":2,"disposition":"declined","detail":"second does not apply"}]`,
			wantCode:  0,
			wantText:  "validated complete finding response account for 2 verdict finding(s) and 0 additional response(s)",
		},
		{
			name:      "incomplete",
			responses: `[{"finding":1,"disposition":"addressed","detail":"fixed first"}]`,
			wantCode:  1,
			wantText:  "verdict finding 2 (second) has no response",
		},
		{
			name: "with in-run additions",
			responses: `[{"finding":1,"disposition":"addressed","detail":"fixed first"},` +
				`{"finding":2,"disposition":"declined","detail":"second does not apply"},` +
				`{"finding":3,"disposition":"addressed","detail":"installed the fake Copilot fixture the in-run reviewer asked for"}]`,
			wantCode: 0,
			wantText: "validated complete finding response account for 2 verdict finding(s) and 1 additional response(s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := initDemo(t)
			t.Setenv("GOOBERS_RUN_ID", runID)
			t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
			t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), "finding-response-validation.json"))
			seedRemediationResponseRunBeforePush(t, root, runID, verdict, tt.responses)

			code, stdout, stderr := runArgs(t, "respond-to-findings", "--check", root)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d; stdout = %q, stderr = %q", code, tt.wantCode, stdout, stderr)
			}
			if output := stdout + stderr; !strings.Contains(output, tt.wantText) {
				t.Errorf("output = %q, want containing %q", output, tt.wantText)
			}
			data, err := os.ReadFile(providerInput("resultFile", remediationResponseArtifactName))
			if err != nil {
				t.Fatalf("read validation result: %v", err)
			}
			var result map[string]interface{}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatalf("unmarshal validation result: %v", err)
			}
			if tt.wantCode == 0 {
				if len(result) != 1 {
					t.Errorf("success result = %v, want only the integrity label", result)
				}
				if got := result["integrity"]; got != string(apiv1.IntegrityUnapproved) {
					t.Errorf("success integrity = %v, want %q", got, apiv1.IntegrityUnapproved)
				}
			}
		})
	}
}

func TestRespondToFindingsCheckPassesDeclaredResultFileExecutorContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper process wrapper uses a POSIX shell")
	}
	const runID = "run-executor-check"
	root := initDemo(t)
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{
			{Severity: apiv1.SeverityError, Message: "first"},
			{Severity: apiv1.SeverityWarning, Message: "second"},
		},
	}
	responses := `[{"finding":1,"disposition":"addressed","detail":"fixed first"},{"finding":2,"disposition":"declined","detail":"second does not apply"}]`
	seedRemediationResponseRunBeforePush(t, root, runID, verdict, responses)

	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "goobers")
	script := "#!/bin/sh\nexec \"$GOOBERS_TEST_BINARY\" -test.run=^TestRespondToFindingsStageHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := journal.DefaultScrubber()
	injector, err := credentials.NewInjector(resolver, nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := executor.NewShellExecutor(injector, respondToFindingsTestRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	shell.InstanceRoot = root
	shell.SelfBin = wrapper

	result, err := shell.Run(
		context.Background(),
		apiv1.InvocationEnvelope{
			TaskID:     "validate-finding-responses",
			WorkflowID: "pr-remediation",
			RunID:      runID,
			Gaggle:     "goobers",
			Workspace:  t.TempDir(),
			Inputs: map[string]interface{}{
				executor.InputResultFile: "finding-response-validation.json",
			},
		},
		apiv1.DeterministicRun{
			Command: []string{"goobers", "respond-to-findings", "--check"},
			Env: map[string]string{
				"GOOBERS_RESPOND_TO_FINDINGS_HELPER_PROCESS": "1",
				"GOOBERS_TEST_BINARY":                        testBinary,
			},
		},
	)
	if err != nil {
		t.Fatalf("execute validation stage: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("validation stage status = %q, want success: %+v", result.Status, result)
	}
	if len(result.Artifacts) != 3 {
		t.Fatalf("validation stage artifacts = %d, want stdout, stderr, and declared result", len(result.Artifacts))
	}
}

func TestRespondToFindingsStageHelperProcess(t *testing.T) {
	if os.Getenv("GOOBERS_RESPOND_TO_FINDINGS_HELPER_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		if i+2 >= len(os.Args) || os.Args[i+1] != "respond-to-findings" {
			os.Exit(2)
		}
		os.Exit(runRespondToFindings(os.Args[i+2:], os.Stdout, os.Stderr))
	}
	os.Exit(2)
}

type respondToFindingsTestRecorder struct{}

func (respondToFindingsTestRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return respondToFindingsTestRecorder{}.RecordArtifactWithIntegrity(name, data, apiv1.IntegrityDerived)
}

func (respondToFindingsTestRecorder) RecordArtifactWithIntegrity(name string, data []byte, integrity apiv1.Integrity) (journal.Ref, error) {
	return journal.Ref{
		Path: name, Digest: journal.Digest(data), Size: int64(len(data)), Integrity: integrity,
	}, nil
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
		{name: "not 1-based", raw: `[{"finding":1,"disposition":"addressed","detail":"a"},{"finding":0,"disposition":"declined","detail":"b"}]`, want: "1-based finding number"},
		{name: "unanswered verdict finding", raw: `[{"finding":1,"disposition":"addressed","detail":"a"},{"finding":3,"disposition":"declined","detail":"b"}]`, want: "verdict finding 2 (second) has no response"},
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

// Remediation causes without a merge review (failing-ci, sibling-overlap)
// carry no verdict, so an implementer that documents its work must not be
// failed for having more than the zero responses the verdict implies.
func TestValidateFindingResponsesWithoutVerdictAcceptsOptionalAccount(t *testing.T) {
	accepted := []struct {
		name string
		raw  string
		want int
	}{
		{name: "silent", raw: "", want: 0},
		{name: "empty array", raw: `[]`, want: 0},
		{
			name: "documents in-run reviewer feedback",
			raw:  `[{"finding":1,"disposition":"addressed","detail":"Installed the fake Copilot fixture the reviewer asked for."}]`,
			want: 1,
		},
		{
			name: "documents several",
			raw: `[{"finding":1,"disposition":"addressed","detail":"Fixed the failing CI job."},` +
				`{"finding":2,"disposition":"declined","detail":"The sibling overlap is intentional."}]`,
			want: 2,
		},
	}
	for _, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			responses, err := validateFindingResponses(nil, tt.raw)
			if err != nil {
				t.Fatalf("validateFindingResponses error = %v, want nil for a verdictless remediation", err)
			}
			if len(responses) != tt.want {
				t.Errorf("responses = %+v, want %d", responses, tt.want)
			}
		})
	}

	rejected := []struct {
		name string
		raw  string
		want string
	}{
		{name: "malformed", raw: "{", want: "decode JSON"},
		{name: "no detail", raw: `[{"finding":1,"disposition":"addressed","detail":" "}]`, want: "no detail"},
		{name: "bad disposition", raw: `[{"finding":1,"disposition":"skipped","detail":"a"}]`, want: "addressed or declined"},
		{name: "not 1-based", raw: `[{"finding":0,"disposition":"addressed","detail":"a"}]`, want: "1-based finding number"},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := validateFindingResponses(nil, tt.raw); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateFindingResponses error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// A reviewer repass raises findings the original verdict never carried; the
// complete verdict account plus those extra responses is a superset, not a
// contract violation.
func TestValidateFindingResponsesAcceptsInRunReviewAdditions(t *testing.T) {
	findings := []apiv1.Finding{
		{Severity: apiv1.SeverityError, Message: "first"},
		{Severity: apiv1.SeverityWarning, Message: "second"},
	}
	responses, err := validateFindingResponses(findings,
		`[{"finding":3,"disposition":"addressed","detail":"answers the in-run reviewer"},`+
			`{"finding":1,"disposition":"addressed","detail":"fixed first"},`+
			`{"finding":2,"disposition":"declined","detail":"second does not apply"}]`)
	if err != nil {
		t.Fatalf("validateFindingResponses error = %v, want nil for a superset account", err)
	}
	if len(responses) != 3 ||
		responses[0].Finding != 1 || responses[1].Finding != 2 || responses[2].Finding != 3 {
		t.Fatalf("responses = %+v, want findings 1, 2, and 3 in order", responses)
	}

	// Every verdict finding still needs its own response; extras do not
	// substitute for one.
	_, err = validateFindingResponses(findings,
		`[{"finding":1,"disposition":"addressed","detail":"fixed first"},`+
			`{"finding":3,"disposition":"addressed","detail":"answers the in-run reviewer"},`+
			`{"finding":4,"disposition":"addressed","detail":"and another"}]`)
	if err == nil || !strings.Contains(err.Error(), "verdict finding 2 (second) has no response") {
		t.Fatalf("validateFindingResponses error = %v, want the unanswered verdict finding named", err)
	}
}

func TestRespondToFindingsPostsAccountWithoutOriginalVerdict(t *testing.T) {
	responses := `[{"finding":1,"disposition":"addressed","detail":"Installed the fake Copilot fixture the in-run reviewer asked for."}]`
	root, server, resultFile := respondToFindingsFixtureForVerdict(t, nil, responses, true)

	code, stdout, stderr := runArgs(t, "respond-to-findings", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	comments := append([]string(nil), server.issues[77].comments...)
	server.mu.Unlock()
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want one run-scoped response", len(comments))
	}
	for _, want := range []string{
		"no merge-review findings to account for",
		"Raised during this remediation cycle",
		"Installed the fake Copilot fixture",
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
	if result.FindingCount != 0 || len(result.Findings) != 1 || result.Findings[0].Original.Message != "" {
		t.Errorf("result = %+v, want a verdictless account with one unbound response", result)
	}
}

func TestRespondToFindingsRecordsInRunAdditionsAlongsideVerdictAccount(t *testing.T) {
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Class:    apiv1.FindingSubstantive,
			Message:  "validate empty input",
			Location: "internal/parser.go:42",
		}},
	}
	responses := `[{"finding":1,"disposition":"addressed","detail":"Added an explicit empty-input guard."},` +
		`{"finding":2,"disposition":"addressed","detail":"Renamed the helper the in-run reviewer flagged."}]`
	root, server, resultFile := respondToFindingsFixture(t, verdict, responses, true)

	code, stdout, stderr := runArgs(t, "respond-to-findings", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	comments := append([]string(nil), server.issues[77].comments...)
	server.mu.Unlock()
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want one run-scoped response", len(comments))
	}
	for _, want := range []string{
		"1. **Addressed** - Added an explicit empty-input guard.",
		"validate empty input",
		"Raised during this remediation cycle",
		"Renamed the helper the in-run reviewer flagged.",
	} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("comment missing %q:\n%s", want, comments[0])
		}
	}
	if strings.Contains(comments[0], "2. **Addressed**") {
		t.Errorf("comment numbers an in-run response as a verdict finding:\n%s", comments[0])
	}

	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read response result: %v", err)
	}
	var result remediationResponseResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal response result: %v", err)
	}
	if result.FindingCount != 1 || len(result.Findings) != 2 {
		t.Fatalf("result = %+v, want one verdict finding and two recorded responses", result)
	}
	if result.Findings[0].Original.Message != "validate empty input" || result.Findings[1].Original.Message != "" {
		t.Errorf("recorded originals = %+v, want only the verdict response bound to a finding", result.Findings)
	}
}
