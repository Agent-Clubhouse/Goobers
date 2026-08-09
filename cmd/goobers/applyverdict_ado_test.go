package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestCloseMootPullRequestDispatchesToADO(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		wantCode   int
		wantError  string
	}{
		{name: "success", statusCode: http.StatusOK},
		{name: "failure", statusCode: http.StatusInternalServerError, wantCode: 1, wantError: "status 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch {
					t.Fatalf("method = %s, want PATCH", r.Method)
				}
				if test.statusCode != http.StatusOK {
					http.Error(w, "close failed", test.statusCode)
					return
				}
				_, _ = w.Write([]byte(`{"pullRequestId":42,"status":"abandoned"}`))
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
				p.BaseURL = server.URL
			})
			var stdout, stderr bytes.Buffer
			resultFile := filepath.Join(t.TempDir(), "verdict-result.json")
			code := closeMootPullRequest(
				context.Background(),
				provider,
				providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"},
				42,
				&providers.PullRequestSummary{Number: 42, HeadSHA: "head", BaseSHA: "base"},
				apiv1.Verdict{Rationale: "No longer needed."},
				"its diff is empty",
				resultFile,
				&stdout,
				&stderr,
			)
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d; stderr = %q", code, test.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.wantError) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), test.wantError)
			}
		})
	}
}

// TestPublishADOPassVerdictPublishesValidationStatus proves the ADO PASS
// transport directly: the verdict rides on a native goobers/validation PR
// status (the surface an ADO status-check branch policy gates on) and the stage
// emits decision=pass so merge-review's published-verdict gate advances. It must
// never fall back to the GitHub UpdateWorkItem(ID: PR#) label write.
func TestPublishADOPassVerdictPublishesValidationStatus(t *testing.T) {
	var (
		statusMethod string
		statusBody   map[string]interface{}
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/statuses", func(w http.ResponseWriter, r *http.Request) {
		statusMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&statusBody)
		_, _ = w.Write([]byte(`{"id":7}`))
	})
	// A PR-as-work-item write would land here (numeric PR id into wit/workitems);
	// it must never be reached on ADO.
	mux.HandleFunc("/org/project/_apis/wit/workitems/359", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("wit/workitems/359 was mutated — the PR-as-work-item wrong-object write ran on ADO")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
	var stdout, stderr bytes.Buffer
	resultFile := filepath.Join(t.TempDir(), "verdict-result.json")
	code := publishADOPassVerdict(
		context.Background(),
		provider,
		providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"},
		359,
		providers.PullRequestSummary{Number: 359, HeadSHA: "head-sha", BaseSHA: "base-sha"},
		resultFile,
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if statusMethod != http.MethodPost {
		t.Fatalf("status method = %q, want POST", statusMethod)
	}
	ctxObj, _ := statusBody["context"].(map[string]interface{})
	if ctxObj["genre"] != "goobers" || ctxObj["name"] != "validation" {
		t.Fatalf("status context = %#v, want genre goobers / name validation", ctxObj)
	}
	if statusBody["state"] != "succeeded" {
		t.Fatalf("status state = %v, want succeeded", statusBody["state"])
	}
	result := readVerdictResult(t, resultFile)
	if result["decision"] != "pass" {
		t.Fatalf("result = %+v, want decision=pass", result)
	}
}

// TestRunApplyVerdictADOPassPublishesStatusAndDecisionPass drives the whole
// stage: it proves runApplyVerdict routes an ADO PASS to the status transport
// (no longer the pre-merge "publishing a non-moot verdict is not supported"
// refusal that killed the run before merge) and emits decision=pass.
func TestRunApplyVerdictADOPassPublishesStatusAndDecisionPass(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)

	const runID = "run-ado-apply-verdict-pass"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "359")
	workDir := t.TempDir()
	t.Chdir(workDir)
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		HeadSHA:  "head-sha",
		BaseSHA:  "base-sha",
	})

	statusPosted := false
	mux := adoMergeReviewMux(t, repo, 359, "head-sha", "base-sha", "active", &statusPosted)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	code, stdout, stderr := runArgs(t, "apply-verdict", root)
	if code != 0 {
		t.Fatalf("apply-verdict: code = %d, want 0; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !statusPosted {
		t.Fatalf("goobers/validation PR status was not published; stdout = %q", stdout)
	}
	result := readVerdictResult(t, filepath.Join(workDir, "verdict-result.json"))
	if result["decision"] != "pass" {
		t.Fatalf("result = %+v, want decision=pass", result)
	}
}

func readVerdictResult(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read verdict result: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal verdict result: %v", err)
	}
	return out
}

// adoMergeReviewMux serves the minimal Azure DevOps REST surface the
// merge-review provider-chain stages poll for a single clean PR: the active-PR
// list, the PR detail (with the project identity policy evaluation needs), an
// empty blocking-policy set, and the PR-status POST (recorded via posted).
func adoMergeReviewMux(t *testing.T, repo providers.RepositoryRef, prNumber int, headSHA, baseSHA, status string, posted *bool) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	mux.HandleFunc(prBase, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})
	mux.HandleFunc(prBase+"/"+strconv.Itoa(prNumber), func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"pullRequestId":         prNumber,
			"status":                status,
			"title":                 "Implement PBI 1456",
			"description":           "Implements PBI 1456\n\nFixes #1456",
			"createdBy":             map[string]string{"displayName": "goober", "uniqueName": "goober@example.com"},
			"isDraft":               false,
			"sourceRefName":         "refs/heads/goobers/tb-ado-implementation/run-359",
			"targetRefName":         "refs/heads/main",
			"lastMergeSourceCommit": map[string]string{"commitId": headSHA},
			"lastMergeTargetCommit": map[string]string{"commitId": baseSHA},
			"reviewers":             []interface{}{},
			"repository": map[string]interface{}{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})
	mux.HandleFunc(prBase+"/"+strconv.Itoa(prNumber)+"/statuses", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("statuses method = %s, want POST", r.Method)
		}
		*posted = true
		_, _ = w.Write([]byte(`{"id":7}`))
	})
	return mux
}

func writeJSONResp(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode json response: %v", err)
	}
}
