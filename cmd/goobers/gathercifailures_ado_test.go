package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// ADO-N22: gather-ci-failures on Azure DevOps reads minimal evidence from the
// pull request's rejected policy evaluations, against a fake ADO server.

func fakeADOCIFailuresServer(t *testing.T, repo providers.RepositoryRef, head string) *httptest.Server {
	t.Helper()
	pr := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests/77"
	mux := http.NewServeMux()
	mux.HandleFunc(pr, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("PR request method = %s, want GET", r.Method)
		}
		writeJSONResp(t, w, map[string]any{
			"pullRequestId": 77, "status": "active", "title": "t",
			"sourceRefName": "refs/heads/goobers/work", "targetRefName": "refs/heads/main",
			"lastMergeSourceCommit": map[string]string{"commitId": head},
			"lastMergeTargetCommit": map[string]string{"commitId": "basebeef"},
			"repository": map[string]any{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("evaluations request method = %s, want GET", r.Method)
		}
		policy := func(typeID, name, status string) map[string]any {
			return map[string]any{"status": status, "configuration": map[string]any{
				"isEnabled": true, "isBlocking": true,
				"type": map[string]string{"id": typeID, "displayName": name},
			}}
		}
		build := policy("0609b952-1397-4640-95ec-e00a01b2c241", "Build", "rejected")
		build["context"] = map[string]any{"buildId": 314}
		writeJSONResp(t, w, map[string]any{"value": []any{
			build,
			policy("fa4e907d-c16b-4a4c-9dfa-4906e5d171dd", "Minimum number of reviewers", "queued"),
		}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

func runGatherCIFailuresOnADO(t *testing.T, head string) (string, providers.RepositoryRef, string) {
	t.Helper()
	const runID = "ado-gather-ci"
	root, repo := adoReviewThreadsStageFixture(t, runID)
	seedGatherCIRun(t, root, runID, remediationBriefFixture(true))
	resultFile := filepath.Join(t.TempDir(), remediationBriefResultFile)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	server := fakeADOCIFailuresServer(t, repo, head)
	t.Cleanup(server.Close)
	routeADOStageProvider(t, server.URL)
	if code, stdout, stderr := runArgs(t, "gather-ci-failures", root); code != 0 {
		t.Fatalf("gather-ci-failures: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	return resultFile, repo, server.URL
}

func TestGatherCIFailuresOnADOWritesPolicyEvidence(t *testing.T) {
	resultFile, repo, serverURL := runGatherCIFailuresOnADO(t, "deadbeef")
	got := readGatherCIBrief(t, resultFile)
	if got.GatherCIFailures == nil || len(got.GatherCIFailures.Checks) != 1 {
		t.Fatalf("CI failures = %#v, want the rejected build policy only (the reviewer wait is not CI)", got.GatherCIFailures)
	}
	check := got.GatherCIFailures.Checks[0]
	wantURL := serverURL + "/" + repo.Owner + "/" + repo.Project + "/_build/results?buildId=314"
	if check.Name != "Build" || check.Conclusion != "rejected" || check.URL != wantURL {
		t.Errorf("check = %#v, want the rejected build with URL %s", check, wantURL)
	}
	if strings.Contains(check.Summary, "STALE") {
		t.Errorf("summary = %q, want no stale marker at the brief's head", check.Summary)
	}
}

func TestGatherCIFailuresOnADOMarksEvidenceFromAnotherHeadStale(t *testing.T) {
	resultFile, _, _ := runGatherCIFailuresOnADO(t, "newhead")
	got := readGatherCIBrief(t, resultFile)
	if got.GatherCIFailures == nil || len(got.GatherCIFailures.Checks) != 1 {
		t.Fatalf("CI failures = %#v, want one check", got.GatherCIFailures)
	}
	summary := got.GatherCIFailures.Checks[0].Summary
	if !strings.HasPrefix(summary, "STALE:") || !strings.Contains(summary, "newhead") || !strings.Contains(summary, "deadbeef") {
		t.Errorf("summary = %q, want a stale marker naming both heads", summary)
	}
}
