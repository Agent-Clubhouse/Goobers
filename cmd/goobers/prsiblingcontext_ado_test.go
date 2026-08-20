package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// TestRunGatherSiblingContextADOEmptySiblingSet proves the ADO gather-sibling-
// context branch: it resolves the selected PR's own deterministic head/base pin
// (which apply-verdict requires) from a single PollPullRequest and emits an
// EMPTY sibling set, so the review gate has trivial no-sibling evidence and the
// run proceeds. It never resolves a github:* token and never runs the GitHub
// sibling scan.
func TestRunGatherSiblingContextADOEmptySiblingSet(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "359")

	statusPosted := false // gather-sibling-context posts no status; the field is unused here.
	mux := adoMergeReviewMux(t, repo, 359, "head-sha", "base-sha", "active", &statusPosted)
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
	code, stdout, stderr := runArgs(t, "gather-sibling-context", root)
	if code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["selectedHeadSha"] != "head-sha" || out["selectedBaseSha"] != "base-sha" {
		t.Fatalf("result = %+v, want selectedHeadSha=head-sha / selectedBaseSha=base-sha from the ADO poll", out)
	}
	if out["selectedNumber"] != "359" {
		t.Fatalf("selectedNumber = %v, want 359 (string)", out["selectedNumber"])
	}
	if out["hasSiblingOverlap"] != "false" {
		t.Fatalf("hasSiblingOverlap = %v, want false", out["hasSiblingOverlap"])
	}
	if sib, ok := out["siblings"].([]interface{}); !ok || len(sib) != 0 {
		t.Fatalf("siblings = %v, want an empty sibling set on ADO", out["siblings"])
	}
	if ov, ok := out["overlappingSiblings"].([]interface{}); !ok || len(ov) != 0 {
		t.Fatalf("overlappingSiblings = %v, want empty on ADO", out["overlappingSiblings"])
	}
}

// TestGatherSiblingContextADOPollFailureKeepsGenericEnvelope is the
// fault-injection coverage the failure-path inventory audit
// (TestGatherSiblingContextFatalProviderPathInventory) requires for the ADO
// branch's PollPullRequest: a provider error must produce the generic,
// non-retryable provider-failure envelope, never a partial success result.
func TestGatherSiblingContextADOPollFailureKeepsGenericEnvelope(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "359")

	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	mux := http.NewServeMux()
	mux.HandleFunc(prBase+"/359", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, _, stderr := runArgs(t, "gather-sibling-context", root)
	if code != 1 {
		t.Fatalf("gather-sibling-context under a 422: code = %d, stderr = %q, want 1", code, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(workDir, "sibling-context.json"))
	if result[executor.OutputErrorCode] != errorCodeProvider {
		t.Fatalf("errorCode = %v, want %s", result[executor.OutputErrorCode], errorCodeProvider)
	}
	if result[executor.OutputErrorRetryable] != false {
		t.Fatalf("errorRetryable = %v, want false", result[executor.OutputErrorRetryable])
	}
	message, _ := result[executor.OutputErrorMessage].(string)
	if !strings.Contains(message, "poll pull request #359") || !strings.Contains(message, "status 422") {
		t.Fatalf("errorMessage = %q, want operation and provider status", message)
	}
	if result["integrity"] != string(apiv1.IntegrityUnapproved) {
		t.Fatalf("integrity = %v, want unapproved", result["integrity"])
	}
	for _, field := range gatherSiblingFailConsumerFields {
		if _, ok := result[field]; ok {
			t.Fatalf("result = %v, provider failure must not synthesize %s", result, field)
		}
	}
}
