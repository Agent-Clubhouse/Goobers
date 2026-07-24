package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func remediationBriefFixture(failing bool) apiv1.RemediationBrief {
	return apiv1.RemediationBrief{
		Schema:                 apiv1.RemediationBriefVersion,
		SelectedNumber:         "77",
		Head:                   "goobers/implementation/run-77",
		Base:                   "main",
		WorkspaceBranch:        "goobers/implementation/run-77",
		IsBehindBase:           false,
		HasSubstantiveFindings: "false",
		HasFailingCI:           fmt.Sprintf("%t", failing),
		GatherPRContext: apiv1.RemediationPRContext{
			HeadSHA:  "deadbeef",
			BaseSHA:  "basebeef",
			Verdict:  nil,
			Comments: []apiv1.RemediationThreadComment{},
		},
		GatherIssueContext: &apiv1.RemediationIssueContext{
			Issues: []apiv1.RemediationIssue{{Number: "938", Title: "CI evidence", Body: "Surface why CI failed."}},
		},
	}
}

func seedGatherCIRun(t *testing.T, root, runID string, brief apiv1.RemediationBrief) {
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
		t.Fatalf("record gather-pr-context result: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close remediation run journal: %v", err)
	}
}

func runGatherCIFixture(t *testing.T, brief apiv1.RemediationBrief) (string, string) {
	t.Helper()
	const runID = "run-prr-2"
	root := initDemo(t)
	seedGatherCIRun(t, root, runID, brief)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	resultFile := filepath.Join(t.TempDir(), remediationBriefResultFile)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	return root, resultFile
}

func readGatherCIBrief(t *testing.T, path string) apiv1.RemediationBrief {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read remediation brief: %v", err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		t.Fatalf("decode remediation brief: %v\n%s", err, data)
	}
	return brief
}

func TestGatherCIFailuresPassingCIAddsNothingAndMakesNoAPICalls(t *testing.T) {
	want := remediationBriefFixture(false)
	root, resultFile := runGatherCIFixture(t, want)

	previous := newGitHubProvider
	newGitHubProvider = func(string, ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		t.Fatal("passing CI must not construct a provider or make API calls")
		return nil
	}
	t.Cleanup(func() { newGitHubProvider = previous })

	code, stdout, stderr := runArgs(t, "gather-ci-failures", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	got := readGatherCIBrief(t, resultFile)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("passing-CI brief changed:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestGatherCIFailuresAddsSummariesAndAnnotationsWithoutRawLogs(t *testing.T) {
	brief := remediationBriefFixture(true)
	root, resultFile := runGatherCIFixture(t, brief)
	var requested []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/your-org/your-repo/commits/deadbeef/status", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		writeFakeJSON(w, map[string]interface{}{"statuses": []map[string]interface{}{{
			"context": "platform gate", "state": "failure",
			"target_url": "https://ci.example/platform", "description": "windows build failed",
		}}})
	})
	mux.HandleFunc("/repos/your-org/your-repo/commits/deadbeef/check-runs", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		writeFakeJSON(w, map[string]interface{}{"check_runs": []map[string]interface{}{
			{
				"id": 501, "name": "unit / ubuntu", "status": "completed", "conclusion": "failure",
				"html_url": "https://ci.example/unit", "output": map[string]interface{}{"summary": "TestWorker failed"},
			},
			{"id": 502, "name": "lint", "status": "completed", "conclusion": "success"},
		}})
	})
	mux.HandleFunc("/repos/your-org/your-repo/check-runs/501/annotations", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		writeFakeJSON(w, []map[string]interface{}{{
			"path": "worker_test.go", "start_line": 42, "end_line": 43,
			"annotation_level": "failure", "title": "TestWorker",
			"message": "expected ready, got pending",
		}})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			t.Errorf("unexpected provider request %s; raw workflow logs must never be fetched", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()

	previous := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = previous })
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")

	code, stdout, stderr := runArgs(t, "gather-ci-failures", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	got := readGatherCIBrief(t, resultFile)
	if !reflect.DeepEqual(got.GatherIssueContext, brief.GatherIssueContext) {
		t.Fatalf("unowned gatherIssueContext changed: got %+v, want %+v", got.GatherIssueContext, brief.GatherIssueContext)
	}
	if got.GatherCIFailures == nil || len(got.GatherCIFailures.Checks) != 2 {
		t.Fatalf("gatherCIFailures = %+v, want two failures", got.GatherCIFailures)
	}
	legacy := got.GatherCIFailures.Checks[0]
	if legacy.Name != "platform gate" || legacy.Conclusion != "failure" ||
		legacy.Summary != "windows build failed" || len(legacy.Annotations) != 0 {
		t.Fatalf("legacy check = %+v, want summary-only failure", legacy)
	}
	check := got.GatherCIFailures.Checks[1]
	if check.Name != "unit / ubuntu" || check.Conclusion != "failure" ||
		check.Summary != "TestWorker failed" || len(check.Annotations) != 1 {
		t.Fatalf("check-run failure = %+v", check)
	}
	annotation := check.Annotations[0]
	if annotation.Path != "worker_test.go" || annotation.StartLine != 42 ||
		annotation.EndLine != 43 || annotation.Level != "failure" ||
		annotation.Title != "TestWorker" || annotation.Message != "expected ready, got pending" {
		t.Fatalf("annotation = %+v", annotation)
	}
	wantRequests := []string{
		"/repos/your-org/your-repo/commits/deadbeef/status",
		"/repos/your-org/your-repo/commits/deadbeef/check-runs",
		"/repos/your-org/your-repo/check-runs/501/annotations",
	}
	if !reflect.DeepEqual(requested, wantRequests) {
		t.Fatalf("requested paths = %v, want %v", requested, wantRequests)
	}
	if gatherCIRawLogByteLimit != 0 {
		t.Fatalf("raw CI log bound = %d bytes, want 0", gatherCIRawLogByteLimit)
	}
}
