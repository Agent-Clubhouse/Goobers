package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// prScopedPoller is a fakePoller that reports CI evidence per pull request
// (providers.PullRequestCIFailureReader) the way Azure DevOps does, and not
// per commit (CIFailureLister).
type prScopedPoller struct {
	fakePoller
	failures []providers.CIFailureDetail
	err      error
	pullIDs  []string
}

func (p *prScopedPoller) PullRequestCIFailures(_ context.Context, _ providers.RepositoryRef, pullID string) (providers.PullRequestCIFailures, error) {
	p.pullIDs = append(p.pullIDs, pullID)
	if p.err != nil {
		return providers.PullRequestCIFailures{}, p.err
	}
	return providers.PullRequestCIFailures{HeadSHA: "head", Failures: p.failures}, nil
}

// #5652: on Azure DevOps the polled checks are policy evaluations named by
// type ("Build"), carrying no summary. The pull-request-scoped evidence names
// them more precisely and carries the build's failed steps. ci-checks.json
// must pair each failing check with its own evidence, including two
// same-named build policies, and a status check matched by its refined name.
func TestCIPollExecutor_PullRequestScopedEvidenceReachesArtifact(t *testing.T) {
	checks := []providers.CheckDetail{
		{Name: "Minimum number of reviewers", State: providers.CheckStatePending},
		{Name: "Build", State: providers.CheckStateFailing, URL: "https://ado.example/b/1"},
		{Name: "Build", State: providers.CheckStateFailing, URL: "https://ado.example/b/2"},
		{Name: "Status", State: providers.CheckStateFailing},
	}
	failures := []providers.CIFailureDetail{
		{
			CheckDetail: providers.CheckDetail{Name: "Build: second", URL: "https://ado.example/b/2", Summary: "evidence complete; second"},
			Annotations: []providers.CheckAnnotation{{Title: "Job / test", Message: "second failure"}},
		},
		{
			CheckDetail: providers.CheckDetail{Name: "Build: first", URL: "https://ado.example/b/1", Summary: "evidence complete; first"},
			Annotations: []providers.CheckAnnotation{{Title: "Job / build", Message: "first failure"}},
		},
		{
			CheckDetail: providers.CheckDetail{Name: "Status: ext/lint", Summary: "evidence unsupported; external status"},
			Annotations: []providers.CheckAnnotation{},
		},
	}
	poller := &prScopedPoller{
		fakePoller: fakePoller{results: []providers.CheckState{providers.CheckStateFailing}, checks: checks},
		failures:   failures,
	}

	artifact := runFailureEvidence(t, poller)
	if len(poller.pullIDs) != 1 || poller.pullIDs[0] != "42" {
		t.Fatalf("PullRequestCIFailures pull ids = %v, want exactly one read of PR 42", poller.pullIDs)
	}
	if len(artifact.Checks) != 4 {
		t.Fatalf("checks = %+v, want the three failing checks then the pending one", artifact.Checks)
	}
	want := []struct{ summary, message string }{
		{"evidence complete; first", "first failure"},
		{"evidence complete; second", "second failure"},
		{"evidence unsupported; external status", ""},
	}
	for i, w := range want {
		got := artifact.Checks[i]
		if got.Summary != w.summary {
			t.Errorf("check %d (%s %s) summary = %q, want %q", i, got.Name, got.URL, got.Summary, w.summary)
		}
		if w.message == "" {
			if len(got.Annotations) != 0 {
				t.Errorf("check %d annotations = %+v, want none", i, got.Annotations)
			}
			continue
		}
		if len(got.Annotations) != 1 || got.Annotations[0].Message != w.message {
			t.Errorf("check %d annotations = %+v, want %q", i, got.Annotations, w.message)
		}
	}
	if pending := artifact.Checks[3]; pending.Summary != "" || len(pending.Annotations) != 0 {
		t.Errorf("pending check = %+v, want no failure evidence attached", pending)
	}
}

// Evidence is best-effort on the pull-request path too: a read error leaves
// the determined failing verdict and its checks intact.
func TestCIPollExecutor_PullRequestScopedEvidenceIsBestEffort(t *testing.T) {
	poller := &prScopedPoller{
		fakePoller: fakePoller{results: []providers.CheckState{providers.CheckStateFailing}, checks: failingChecksFixture()},
		err:        errors.New("evidence unavailable"),
	}
	artifact := runFailureEvidence(t, poller)
	if len(artifact.Checks) != 1 || len(artifact.Checks[0].Annotations) != 0 {
		t.Fatalf("checks = %+v, want the failing check recorded despite the evidence error", artifact.Checks)
	}
}

// A check that carries its own summary keeps it; the evidence summary only
// fills an empty one.
func TestCIEvidenceForPullRequestKeepsOwnSummary(t *testing.T) {
	checks := []providers.CheckDetail{{Name: "Build", State: providers.CheckStateFailing, Summary: "own"}}
	failures := []providers.CIFailureDetail{{CheckDetail: providers.CheckDetail{Name: "Build #7", Summary: "evidence"}}}
	data, err := marshalCIChecksArtifactWith(checks, ciEvidenceForPullRequest(checks, failures), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, `"summary":"own"`) {
		t.Fatalf("artifact = %s, want the check's own summary kept", got)
	}
}

// fakeADOBuildServer serves PR 42 with one rejected build policy whose build
// 314 failed in its "go test" task (#5652).
func fakeADOBuildServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	serve := func(path string, v any) {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Errorf("encode: %v", err)
			}
		})
	}
	serve("/org/project/_apis/git/repositories/r/pullrequests/42", map[string]any{
		"pullRequestId": 42, "status": "active",
		"lastMergeSourceCommit": map[string]string{"commitId": "head-sha"},
		"repository": map[string]any{"id": "repo-guid", "name": "repo",
			"project": map[string]string{"id": "proj-guid", "name": "project"}},
	})
	serve("/org/project/_apis/policy/evaluations", map[string]any{"value": []any{map[string]any{
		"status":  "rejected",
		"context": map[string]any{"buildId": 314},
		"configuration": map[string]any{"isEnabled": true, "isBlocking": true,
			"type":     map[string]string{"id": "0609b952-1397-4640-95ec-e00a01b2c241", "displayName": "Build"},
			"settings": map[string]any{"displayName": "ci"}},
	}}})
	serve("/org/project/_apis/build/builds/314", map[string]any{"id": 314, "buildNumber": "1", "result": "failed",
		"triggerInfo": map[string]string{"pr.number": "42", "pr.sourceSha": "head-sha"},
		"repository":  map[string]string{"id": "repo-guid"}})
	serve("/org/project/_apis/build/builds/314/timeline", map[string]any{"records": []any{
		map[string]any{"id": "j", "type": "Job", "name": "Linux", "result": "failed"},
		map[string]any{"id": "t", "parentId": "j", "type": "Task", "name": "go test", "result": "failed",
			"log":    map[string]int{"id": 7},
			"issues": []any{map[string]any{"type": "error", "message": "undefined: widgetCount"}}},
	}})
	serve("/org/project/_apis/build/builds/314/logs", map[string]any{"value": []any{map[string]int{"id": 7, "lineCount": 2}}})
	serve("/org/project/_apis/build/builds/314/logs/7", map[string]any{"value": []string{"--- FAIL: TestWidget", "##[error]Bash exited with code '1'."}})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

// The field failure behind #5652: an implementation run's remediate-ci read
// ci-poll's evidence for a failed Azure DevOps build and found only the
// policy name "Build". With the real ADO provider as the poller, the failing
// check now carries the failed task's issue and log excerpt.
func TestCIPollExecutor_ADOFailedBuildEvidenceReachesArtifact(t *testing.T) {
	server := fakeADOBuildServer(t)
	defer server.Close()
	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })

	artifact := runFailureEvidence(t, provider)
	if len(artifact.Checks) != 1 {
		t.Fatalf("checks = %+v, want the rejected build policy", artifact.Checks)
	}
	check := artifact.Checks[0]
	if check.Name != "Build" || !strings.Contains(check.Summary, "evidence complete") {
		t.Errorf("check = %+v, want the polled Build check with the graded evidence summary", check.CheckDetail)
	}
	if len(check.Annotations) != 2 || check.Annotations[0].Message != "undefined: widgetCount" ||
		!strings.Contains(check.Annotations[1].Message, "--- FAIL: TestWidget") {
		t.Fatalf("annotations = %+v, want the failed task's issue and log excerpt", check.Annotations)
	}
}
