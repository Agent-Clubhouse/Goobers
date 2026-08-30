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
// status (the surface an ADO status-check branch policy gates on) and — so the
// merge commit merge-pr later builds keeps its reviewer attribution and
// rationale (#2746) — on a SHA-pinned PR thread comment, and the stage emits
// decision=pass so merge-review's published-verdict gate advances. It must
// never fall back to the GitHub UpdateWorkItem(ID: PR#) label write.
func TestPublishADOPassVerdictPublishesValidationStatus(t *testing.T) {
	var (
		statusMethod string
		statusBody   map[string]interface{}
		threadMethod string
		threadBody   map[string]interface{}
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/statuses", func(w http.ResponseWriter, r *http.Request) {
		statusMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&statusBody)
		_, _ = w.Write([]byte(`{"id":7}`))
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/threads", func(w http.ResponseWriter, r *http.Request) {
		threadMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&threadBody)
		_, _ = w.Write([]byte(`{"id":11,"comments":[{"id":1,"content":"posted","commentType":"text"}]}`))
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
		apiv1.Verdict{
			Decision:  apiv1.VerdictPass,
			Summary:   "Clean parity fix.",
			Rationale: "Every conjunct is re-checked in-lock.",
		},
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
	if threadMethod != http.MethodPost {
		t.Fatalf("thread method = %q, want POST (the pass verdict must reach the PR thread)", threadMethod)
	}
	comments, _ := threadBody["comments"].([]interface{})
	if len(comments) != 1 {
		t.Fatalf("thread body = %+v, want one comment", threadBody)
	}
	content, _ := comments[0].(map[string]interface{})["content"].(string)
	posted := parsePostedVerdictComment(t, content)
	if posted.Decision != apiv1.VerdictPass || posted.Rationale != "Every conjunct is re-checked in-lock." {
		t.Fatalf("posted verdict = %+v, want the pass decision and rationale", posted)
	}
	if posted.HeadSHA != "head-sha" || posted.BaseSHA != "base-sha" {
		t.Fatalf("posted verdict = %+v, want SHA-pinned to the reviewed head/base", posted)
	}
	result := readVerdictResult(t, resultFile)
	if result["decision"] != "pass" {
		t.Fatalf("result = %+v, want decision=pass", result)
	}
}

// parsePostedVerdictComment recovers the machine-readable verdict payload from a
// rendered merge-review verdict comment.
func parsePostedVerdictComment(t *testing.T, body string) apiv1.Verdict {
	t.Helper()
	verdict, ok := parseVerdictComment(body)
	if !ok {
		t.Fatalf("verdict comment = %q, want a parsable verdict-json payload", body)
	}
	return verdict
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
	mux.HandleFunc(prBase+"/"+strconv.Itoa(prNumber)+"/threads", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"id":       11,
			"comments": []interface{}{map[string]interface{}{"id": 1, "content": "", "commentType": "text"}},
		})
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

// TestPublishADONonPassVerdictPublishesFailedStatusLabelAndThread proves the ADO
// non-pass transport directly (the symmetric counterpart to the PASS test): a
// needs-changes verdict rides on a FAILED goobers/validation PR status, the
// goobers:needs-remediation PR label written via the native PR-labels endpoint,
// and the findings + verdict-json posted to a PR thread — and emits
// decision=needs-changes so merge-review's published-verdict gate routes away
// from merge. It must never fall back to the GitHub UpdateWorkItem(ID: PR#) label
// write (the wrong-object hazard).
func TestPublishADONonPassVerdictPublishesFailedStatusLabelAndThread(t *testing.T) {
	var (
		statusMethod  string
		statusBody    map[string]interface{}
		labelMethod   string
		labelBody     map[string]interface{}
		threadMethod  string
		threadContent string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/statuses", func(w http.ResponseWriter, r *http.Request) {
		statusMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&statusBody)
		_, _ = w.Write([]byte(`{"id":7}`))
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/labels", func(w http.ResponseWriter, r *http.Request) {
		// GET is the escalation-suppression label read; return no labels so the
		// needs-changes verdict routes to remediation (not suppressed).
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"value":[]}`))
			return
		}
		labelMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&labelBody)
		_, _ = w.Write([]byte(`{"id":"label-guid","name":"goobers:needs-remediation"}`))
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/threads", func(w http.ResponseWriter, r *http.Request) {
		threadMethod = r.Method
		var payload struct {
			Comments []struct {
				Content string `json:"content"`
			} `json:"comments"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if len(payload.Comments) > 0 {
			threadContent = payload.Comments[0].Content
		}
		_, _ = w.Write([]byte(`{"id":11,"comments":[{"id":1,"content":"posted","author":{"displayName":"goober"},"publishedDate":"2026-08-09T00:00:00Z"}]}`))
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
	code := publishADONonPassVerdict(
		context.Background(),
		provider,
		providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"},
		359,
		providers.PullRequestSummary{Number: 359, HeadSHA: "head-sha", BaseSHA: "base-sha"},
		apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "Fix the off-by-one.", HeadSHA: "head-sha", BaseSHA: "base-sha"},
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
	if statusBody["state"] != "failed" {
		t.Fatalf("status state = %v, want failed", statusBody["state"])
	}
	if labelMethod != http.MethodPost {
		t.Fatalf("label method = %q, want POST", labelMethod)
	}
	if labelBody["name"] != needsRemediationLabel {
		t.Fatalf("label name = %v, want %q", labelBody["name"], needsRemediationLabel)
	}
	if threadMethod != http.MethodPost {
		t.Fatalf("thread method = %q, want POST", threadMethod)
	}
	if !strings.Contains(threadContent, "verdict-json:") {
		t.Fatalf("thread content = %q, want it to carry the verdict-json machine payload", threadContent)
	}
	result := readVerdictResult(t, resultFile)
	if result["decision"] != "needs-changes" {
		t.Fatalf("result = %+v, want decision=needs-changes", result)
	}
}

// TestPublishADOFailVerdictEscalatesAndClearsRemediation pins the escalate/park
// contract (§4 D2): a fail verdict is NEVER burned on the remediation budget —
// it applies goobers:merge-escalated and clears goobers:needs-remediation so the
// PR parks for a human instead of re-entering pr-remediation. Removal is
// delete-by-id (colon-name delete 400s on ADO), so the stage first reads the
// labels to resolve the id.
func TestPublishADOFailVerdictEscalatesAndClearsRemediation(t *testing.T) {
	var (
		addedLabel   string
		removedLabel string
		statusState  string
	)
	const nrID = "nr-label-guid"
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/statuses", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		statusState, _ = body["state"].(string)
		_, _ = w.Write([]byte(`{"id":7}`))
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/labels", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// The PR carries needs-remediation from a prior needs-changes cycle;
			// the fail must clear it. Return it with an id so remove-by-id resolves.
			_, _ = w.Write([]byte(`{"value":[{"id":"` + nrID + `","name":"` + needsRemediationLabel + `"}]}`))
		case http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			addedLabel, _ = body["name"].(string)
			_, _ = w.Write([]byte(`{"id":"esc-guid","name":"` + remediationEscalatedLabel + `"}`))
		default:
			t.Fatalf("labels method = %s, want GET or POST", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/labels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("label delete method = %s, want DELETE", r.Method)
		}
		if id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/org/project/_apis/git/repositories/repo/pullrequests/359/labels/"), "/"); id == nrID {
			removedLabel = needsRemediationLabel
		} else {
			t.Fatalf("delete by id = %q, want the needs-remediation id %q", id, nrID)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/359/threads", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":11,"comments":[{"id":1,"content":"posted"}]}`))
	})
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
	code := publishADONonPassVerdict(
		context.Background(),
		provider,
		providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"},
		359,
		providers.PullRequestSummary{Number: 359, HeadSHA: "head-sha", BaseSHA: "base-sha"},
		apiv1.Verdict{Decision: apiv1.VerdictFail, Summary: "Fundamentally wrong approach.", HeadSHA: "head-sha", BaseSHA: "base-sha"},
		resultFile,
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if statusState != "failed" {
		t.Fatalf("status state = %q, want failed", statusState)
	}
	if addedLabel != remediationEscalatedLabel {
		t.Fatalf("added label = %q, want %q (fail escalates, not remediates)", addedLabel, remediationEscalatedLabel)
	}
	if removedLabel != needsRemediationLabel {
		t.Fatalf("removed label = %q, want %q (fail clears remediation, never burns the budget)", removedLabel, needsRemediationLabel)
	}
	result := readVerdictResult(t, resultFile)
	if result["decision"] != "fail" {
		t.Fatalf("result = %+v, want decision=fail", result)
	}
}

// TestRunApplyVerdictADONeedsChangesBridgesToRemediation drives the whole stage:
// it proves runApplyVerdict routes an ADO needs-changes verdict through the
// Part-1 bridge (the injection that replaced the "publishing a non-moot verdict
// is not supported" hard-fail) — a failed goobers/validation status, the
// needs-remediation PR label, the verdict-json on a PR thread — and emits
// decision=needs-changes so the run completes cleanly.
func TestRunApplyVerdictADONeedsChangesBridgesToRemediation(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)

	const runID = "run-ado-apply-verdict-needs-changes"
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "359")
	workDir := t.TempDir()
	t.Chdir(workDir)
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "Fix the off-by-one in the loop bound.",
		HeadSHA:  "head-sha",
		BaseSHA:  "base-sha",
	})

	var statusState, labelName, threadContent string
	mux := adoNeedsChangesMux(t, repo, 359, "head-sha", "base-sha", &statusState, &labelName, &threadContent)
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
	if statusState != "failed" {
		t.Fatalf("goobers/validation status state = %q, want failed; stdout = %q", statusState, stdout)
	}
	if labelName != needsRemediationLabel {
		t.Fatalf("PR label = %q, want %q", labelName, needsRemediationLabel)
	}
	if !strings.Contains(threadContent, "verdict-json:") {
		t.Fatalf("thread content = %q, want the verdict-json machine payload", threadContent)
	}
	result := readVerdictResult(t, filepath.Join(workDir, "verdict-result.json"))
	if result["decision"] != "needs-changes" {
		t.Fatalf("result = %+v, want decision=needs-changes", result)
	}
}

// adoNeedsChangesMux serves the Azure DevOps REST surface a needs-changes
// apply-verdict run touches: the active-PR list, the PR detail (with an
// issue-ref-free body so the moot-close path is not taken), an empty
// blocking-policy set, the PR iteration + changes (a non-empty diff, so the PR
// is not treated as moot), and the three non-pass write surfaces — the status
// POST, the label POST, and the thread POST — each recording what it received.
// A wit/workitems write for the PR number fails the test (the wrong-object
// hazard must never run on ADO).
func adoNeedsChangesMux(t *testing.T, repo providers.RepositoryRef, prNumber int, headSHA, baseSHA string, statusState, labelName, threadContent *string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	pr := prBase + "/" + strconv.Itoa(prNumber)
	mux.HandleFunc(prBase, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})
	mux.HandleFunc(pr, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"pullRequestId":         prNumber,
			"status":                "active",
			"title":                 "Add a widget",
			"description":           "Add a widget to the dashboard.",
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
	mux.HandleFunc(pr+"/iterations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{map[string]int{"id": 1}}})
	})
	mux.HandleFunc(pr+"/iterations/1/changes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"changeEntries": []interface{}{
				map[string]interface{}{"changeType": "edit", "item": map[string]string{"path": "/widget.go"}},
			},
			"nextSkip": 0,
		})
	})
	mux.HandleFunc(pr+"/statuses", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("statuses method = %s, want POST", r.Method)
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, ok := body["state"].(string); ok {
			*statusState = s
		}
		_, _ = w.Write([]byte(`{"id":7}`))
	})
	mux.HandleFunc(pr+"/labels", func(w http.ResponseWriter, r *http.Request) {
		// GET is the escalation-suppression label read; return no labels so the
		// needs-changes verdict routes to remediation (not suppressed).
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"value":[]}`))
			return
		}
		if r.Method != http.MethodPost {
			t.Fatalf("labels method = %s, want GET or POST", r.Method)
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if n, ok := body["name"].(string); ok {
			*labelName = n
		}
		_, _ = w.Write([]byte(`{"id":"label-guid","name":"goobers:needs-remediation"}`))
	})
	mux.HandleFunc(pr+"/threads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("threads method = %s, want POST", r.Method)
		}
		var payload struct {
			Comments []struct {
				Content string `json:"content"`
			} `json:"comments"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if len(payload.Comments) > 0 {
			*threadContent = payload.Comments[0].Content
		}
		writeJSONResp(t, w, map[string]interface{}{
			"id": 11,
			"comments": []interface{}{
				map[string]interface{}{"id": 1, "content": "posted", "author": map[string]string{"displayName": "goober"}, "publishedDate": "2026-08-09T00:00:00Z"},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitems/"+strconv.Itoa(prNumber), func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("wit/workitems/%d was mutated — the PR-as-work-item wrong-object write ran on ADO", prNumber)
	})
	return mux
}
