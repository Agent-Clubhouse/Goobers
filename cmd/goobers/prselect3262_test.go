package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// TestPRSelectAlwaysExcludesNeedsHumanLabel is #3262's fix: a PR labeled
// goobers:needs-human — the label the #2947 failure-streak circuit breaker
// applies to a claimed item's driving PR after failureStreakThreshold
// consecutive terminal failures — must never be eligible for merge-review
// reselection, even if a caller's excludeLabels input omits it, same
// always-on treatment as noMergeReviewLabel and abortedRunLabel (#2238).
// Before this fix pr-select never checked the label at all, so the breaker's
// park action was inert for PR reselection: #607 was reselected and failed
// the same way 112 times because nothing pr-select consulted ever excluded
// it (production evidence in #3262).
func TestPRSelectAlwaysExcludesNeedsHumanLabel(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(3262, "goobers/implementation/run-3262", "main", "needs-human-head", "main-base", false, []string{providers.LabelNeedsHuman}, nil)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want no work", code, stdout, stderr)
	}
	assertNoWorkProviderStageResult(t, resultFile)
}

// TestPRSelectADOAlwaysExcludesNeedsHumanLabel verifies that ADO's
// branch-policy source reaches the same shared exclusion decision as the
// GitHub/Gitea ref-check source. The failure-streak breaker itself cannot trip
// on ADO today (#3004), but an operator can still apply goobers:needs-human by
// hand, and pr-select must respect it there too.
func TestPRSelectADOAlwaysExcludesNeedsHumanLabel(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	server := adoPRSelectServerWithLabels(t, "approved", []string{providers.LabelNeedsHuman})
	adoPRSelectEnv(t, repo, server)
	t.Setenv("GOOBERS_INPUT_HEADPREFIXES", "goobers/tb-ado-implementation/")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no eligible") && !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no-work: goobers:needs-human must exclude PR #359 on the ADO branch too", stdout)
	}
}

// adoPRSelectServerWithLabels is adoPRSelectServer plus a labels array on the
// fixture PR, so a test can prove pr-select's ADO branch respects PR labels
// end to end (not just via the shared hasPRSelectExclusion unit itself).
func adoPRSelectServerWithLabels(t *testing.T, policyStatus string, labels []string) *httptest.Server {
	t.Helper()
	type adoLabel struct {
		Name string `json:"name"`
	}
	adoLabels := make([]adoLabel, len(labels))
	for i, name := range labels {
		adoLabels[i] = adoLabel{Name: name}
	}
	labelsJSON, err := json.Marshal(adoLabels)
	if err != nil {
		t.Fatalf("marshal labels: %v", err)
	}
	prJSON := fmt.Sprintf(`{
		"pullRequestId": 359,
		"status": "active",
		"title": "ADO PR 359",
		"description": "Fixes #1456",
		"sourceRefName": "refs/heads/goobers/tb-ado-implementation/1456",
		"targetRefName": "refs/heads/main",
		"isDraft": false,
		"createdBy": {"uniqueName": "goobers-bot"},
		"lastMergeSourceCommit": {"commitId": "h359"},
		"lastMergeTargetCommit": {"commitId": "base359"},
		"repository": {"id": "repo-guid", "name": "web", "project": {"id": "proj-guid", "name": "project"}},
		"_links": {"web": {"href": "https://dev.azure.test/pr/359"}},
		"labels": %s
	}`, labelsJSON)
	mux := http.NewServeMux()
	mux.HandleFunc("/acme/project/_apis/git/repositories/web/pullrequests", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"value": [%s]}`, prJSON)
	})
	mux.HandleFunc("/acme/project/_apis/git/repositories/web/pullrequests/359", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(prJSON))
	})
	mux.HandleFunc("/acme/project/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{{
				"status": policyStatus,
				"configuration": map[string]any{
					"id":         1,
					"isEnabled":  true,
					"isBlocking": true,
					"type":       map[string]any{"displayName": "Build"},
				},
			}},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}
