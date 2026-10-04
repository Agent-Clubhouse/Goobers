package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// adoIssueContextServer is a fake Azure DevOps server for gather-issue-context:
// pull request 77 (with the given status, target and description) and work
// item 945. Every other work item is not found.
func adoIssueContextServer(t *testing.T, repo providers.RepositoryRef, status, target, description string) *httptest.Server {
	t.Helper()
	project := "/" + repo.Owner + "/" + repo.Project
	mux := http.NewServeMux()
	mux.HandleFunc(project+"/_apis/git/repositories/"+repo.Name+"/pullrequests/77", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("pull request method = %s, want GET", r.Method)
		}
		writeJSONResp(t, w, map[string]any{
			"pullRequestId": 77, "status": status, "title": "t", "description": description,
			"sourceRefName": "refs/heads/goobers/implementation/run-77", "targetRefName": "refs/heads/" + target,
			"lastMergeSourceCommit": map[string]string{"commitId": "head-sha"},
			"lastMergeTargetCommit": map[string]string{"commitId": "base-sha"},
			"createdBy":             map[string]string{"displayName": "goober"},
			"repository": map[string]any{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc(project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]any{"value": []any{}})
	})
	mux.HandleFunc(project+"/_apis/wit/workitems/945", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]any{
			"id": 945,
			"fields": map[string]any{
				"System.WorkItemType":                      "Task",
				"System.Title":                             "Originating work item",
				"System.State":                             "Active",
				"System.Description":                       "Implement the requested behavior.",
				"Microsoft.VSTS.Common.AcceptanceCriteria": "- Include this body.",
			},
		})
	})
	mux.HandleFunc(project+"/_apis/wit/workitemtypes/Task/states", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]any{"value": []map[string]string{{"name": "Active", "category": "InProgress"}}})
	})
	mux.HandleFunc(project+"/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

// routeADOIssueContextProviders points every ADO stage provider at serverURL
// and records, in order, the capability each was built from.
func routeADOIssueContextProviders(t *testing.T, serverURL string) *[]string {
	t.Helper()
	var consumed []string
	original := newADOProviderForStage
	newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		consumed = append(consumed, deliveredCapabilityOf(t, credential))
		provider, err := buildADOProviderForStage(routed, credential)
		if err != nil {
			return nil, err
		}
		provider.BaseURL = serverURL
		return provider, nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })
	return &consumed
}

func readIssueContextResult(t *testing.T, dir string) apiv1.RemediationBrief {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, remediationBriefResultFile))
	if err != nil {
		t.Fatal(err)
	}
	var got apiv1.RemediationBrief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestGatherIssueContextOnADO: the shipped pr-remediation runs
// gather-issue-context unconditionally, so on Azure DevOps it must read the
// selected pull request's description (the list omits it) and resolve its
// closing references to work items, each provider built from its own
// declared capability's credential.
func TestGatherIssueContextOnADO(t *testing.T) {
	const runID = "ado-issue-context"
	root, repo := adoReviewThreadsStageFixture(t, runID)
	seedRemediationBriefRun(t, root, runID, issueContextBrief())
	server := adoIssueContextServer(t, repo, "active", "main", "Implements #111\n\nFixes #945\nFixes #946")
	defer server.Close()
	consumed := routeADOIssueContextProviders(t, server.URL)
	dir := t.TempDir()
	t.Chdir(dir)

	code, stdout, stderr := runArgs(t, "gather-issue-context", root)
	if code != 0 {
		t.Fatalf("gather-issue-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if want := []string{credPRWrite, credIssuesRead}; !reflect.DeepEqual(*consumed, want) {
		t.Fatalf("ADO providers built from %v, want %v", *consumed, want)
	}
	got := readIssueContextResult(t, dir)
	if got.GatherIssueContext == nil || len(got.GatherIssueContext.Issues) != 1 {
		t.Fatalf("issue context = %#v, want the one resolvable closing work item", got.GatherIssueContext)
	}
	issue := got.GatherIssueContext.Issues[0]
	if issue.Number != "945" || issue.Title != "Originating work item" ||
		issue.Body != "Implement the requested behavior.\n\n## Acceptance Criteria\n\n- Include this body." {
		t.Fatalf("issue = %#v", issue)
	}
	if got.SelectedNumber != "77" || got.GatherPRContext.HeadSHA != "head-sha" {
		t.Fatalf("brief fields were not preserved: %#v", got)
	}
	if !strings.Contains(stderr, "originating issue #946 no longer resolves") {
		t.Fatalf("stderr = %q, want the unresolvable work item named", stderr)
	}
}

// TestGatherIssueContextOnADOSkipsPullRequestThatNoLongerResolves: a pull
// request that is no longer active, or now targets another base, yields an
// empty issue context exactly as a pull request missing from the other
// providers' open-into-base list does.
func TestGatherIssueContextOnADOSkipsPullRequestThatNoLongerResolves(t *testing.T) {
	for name, tc := range map[string]struct{ status, target string }{
		"completed":  {status: "completed", target: "main"},
		"retargeted": {status: "active", target: "release"},
	} {
		t.Run(name, func(t *testing.T) {
			runID := "ado-issue-context-" + name
			root, repo := adoReviewThreadsStageFixture(t, runID)
			seedRemediationBriefRun(t, root, runID, issueContextBrief())
			server := adoIssueContextServer(t, repo, tc.status, tc.target, "Fixes #945")
			defer server.Close()
			routeADOIssueContextProviders(t, server.URL)
			dir := t.TempDir()
			t.Chdir(dir)

			code, stdout, stderr := runArgs(t, "gather-issue-context", root)
			if code != 0 {
				t.Fatalf("gather-issue-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			got := readIssueContextResult(t, dir)
			if got.GatherIssueContext == nil || len(got.GatherIssueContext.Issues) != 0 {
				t.Fatalf("issue context = %#v, want empty", got.GatherIssueContext)
			}
			if !strings.Contains(stderr, "selected PR #77 no longer resolves") {
				t.Fatalf("stderr = %q, want the unresolved PR named", stderr)
			}
		})
	}
}
