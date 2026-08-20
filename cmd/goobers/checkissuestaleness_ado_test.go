package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
