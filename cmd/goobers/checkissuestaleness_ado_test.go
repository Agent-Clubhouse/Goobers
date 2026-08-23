package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// TestCheckIssueStalenessADONoPinIsNeverStaleWithoutMutation is the ADO analogue
// of the no-pin compatibility floor (#2340) and PR 359's actual case: the PR
// body carries no issue-spec pin, so the stage reads only the body, emits
// issueStale=false, and performs ZERO work-item mutation. In particular it must
// never run the GitHub stale-branch UpdateWorkItem(ID: pullNumber) write, which
// on ADO addresses wit/workitems/359 and would corrupt the unrelated work item
// sharing the PR's numeric id (the wrong-object hazard).
func TestCheckIssueStalenessADONoPinIsNeverStaleWithoutMutation(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_PULLNUMBER", "359")

	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	mux := http.NewServeMux()
	mux.HandleFunc(prBase+"/359", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"pullRequestId": 359,
			"status":        "active",
			// NO issue-spec-pin marker anywhere in the body — PR 359's case.
			"description":           "Implements PBI 1456\n\nFixes #1456",
			"sourceRefName":         "refs/heads/goobers/tb-ado-implementation/run-359",
			"targetRefName":         "refs/heads/main",
			"lastMergeSourceCommit": map[string]string{"commitId": "head-sha"},
			"lastMergeTargetCommit": map[string]string{"commitId": "base-sha"},
			"repository": map[string]interface{}{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})
	// The GitHub stale-branch write would PATCH wit/workitems/359 (PR number into
	// wit/workitems) — it must never be reached on ADO.
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitems/359", func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("wit/workitems/359 %s — check-issue-staleness ran the PR-as-work-item write on ADO", r.Method)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	dir := t.TempDir()
	t.Chdir(dir)
	code, stdout, stderr := runArgs(t, "check-issue-staleness", root)
	if code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "false" {
		t.Fatalf("result = %+v, want issueStale=false for a no-pin ADO PR", result)
	}
	if result["number"] != "359" {
		t.Fatalf("result = %+v, want number=359 passthrough", result)
	}
}

// TestCheckIssueStalenessADODetectsStaleAndDoesNotWriteRemediationLabel is the
// ADO-specific stale-detection test: when a pinned work item is stale, the stage
// must report issueStale=true but NOT execute the GitHub UpdateWorkItem(ID:
// pullNumber) write, which would corrupt the unrelated work item sharing the
// PR's numeric ID on ADO. The work item read uses backlogRepoRefForStage which
// can route to a different project; the mock serves work item reads from both
// projects to handle both cases.
func TestCheckIssueStalenessADODetectsStaleAndDoesNotWriteRemediationLabel(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_PULLNUMBER", "360")

	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	mux := http.NewServeMux()

	snapshotAt := time.Now().UTC().Add(-2 * time.Hour)
	updatedAfterSnapshot := time.Now().UTC()
	// Use legacy timestamp-only pin format (no specDigest) to test timestamp-based staleness detection
	pin := `<!-- issue-spec-pin: {"issueId":"1457","updatedAt":"` + snapshotAt.Format(time.RFC3339) + `"} -->`

	mux.HandleFunc(prBase+"/360", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"pullRequestId": 360,
			"status":        "active",
			"description":   "Implements PBI 1457\n\nFixes #1457\n" + pin,
			"sourceRefName": "refs/heads/goobers/tb-ado-implementation/run-360",
			"targetRefName": "refs/heads/main",
			"lastMergeSourceCommit": map[string]string{"commitId": "head-sha"},
			"lastMergeTargetCommit": map[string]string{"commitId": "base-sha"},
			"repository": map[string]interface{}{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})

	// Mock the work item read for project (the code repo project). When
	// backlogRepoRefForStage is called without GOOBERS_GAGGLE, it returns the
	// routed repo unchanged, so the work item read targets the code project.
	// The work item's System.ChangedDate is after snapshotAt, making it stale.
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"value": []map[string]interface{}{
				{
					"name":     "Active",
					"category": "Proposed",
				},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitems/1457", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSONResp(t, w, map[string]interface{}{
				"id":     1457,
				"rev":    1,
				"url":    "https://dev.azure.com/acme/project/_apis/wit/workitems/1457",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.Title":        "Updated title",
					"System.Description": "Updated body",
					"System.ChangedDate": updatedAfterSnapshot.Format(time.RFC3339Nano),
					"System.State":       "Active",
				},
			})
		} else {
			t.Errorf("unexpected method %s on workitems/1457", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})

	// The PR-as-work-item write would PATCH wit/workitems/360 (PR number into
	// wit/workitems) — it must never be reached on ADO.
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitems/360", func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("wit/workitems/360 %s — check-issue-staleness ran the PR-as-work-item write on ADO when stale", r.Method)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	dir := t.TempDir()
	t.Chdir(dir)
	code, stdout, stderr := runArgs(t, "check-issue-staleness", root)
	if code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "true" {
		t.Fatalf("result = %+v, want issueStale=true when work item is stale on ADO", result)
	}
	if result["number"] != "360" {
		t.Fatalf("result = %+v, want number=360", result)
	}
	reason, ok := result["reason"].(string)
	if !ok || !strings.Contains(reason, "1457") {
		t.Fatalf("reason = %q, want it to mention work item #1457", reason)
	}
	if !strings.Contains(stdout, "reporting issueStale without mutation") {
		t.Fatalf("stdout = %q, want warning about ADO remediation write being skipped", stdout)
	}
}
