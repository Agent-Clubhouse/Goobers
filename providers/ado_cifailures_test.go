package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestADOPullRequestCIFailuresReportsRejectedCIPolicies is ADO-N22 (design
// ado-parity-dsl-2-0.md §3.4): rejected and broken blocking build and status
// policies are CI failure evidence with a build link from context.buildId;
// reviewer, comment, work-item, advisory, passing and not-applicable
// evaluations are not.
func TestADOPullRequestCIFailuresReportsRejectedCIPolicies(t *testing.T) {
	build := buildPolicy("rejected", 314)
	build["configuration"].(map[string]interface{})["settings"] = map[string]interface{}{"displayName": "ci-validate"}
	status := typedPolicy(adoPolicyTypeStatus, "Status", "broken")
	status["configuration"].(map[string]interface{})["settings"] = map[string]interface{}{
		"statusGenre": "example-ci", "statusName": "lint",
	}
	unnamed := buildPolicy("rejected", 315)
	unnamed["configuration"].(map[string]interface{})["id"] = 27
	other := typedPolicy("00000000-0000-0000-0000-00000000abcd", "Custom policy", "rejected")
	advisory := buildPolicy("rejected", 99)
	advisory["configuration"].(map[string]interface{})["isBlocking"] = false
	evaluations := []map[string]interface{}{
		build,
		status,
		unnamed,
		other,
		typedPolicy(adoPolicyTypeMinimumReviewers, "Minimum number of reviewers", "queued"),
		typedPolicy(adoPolicyTypeRequiredReviewers, "Required reviewers", "rejected"),
		typedPolicy(adoPolicyTypeCommentRequirements, "Comment requirements", "rejected"),
		typedPolicy(adoPolicyTypeWorkItemLinking, "Work item linking", "rejected"),
		typedPolicy(adoPolicyTypeStatus, "Status", "notApplicable"),
		buildPolicy("approved", 7),
		advisory,
	}

	var requests []string
	mux := http.NewServeMux()
	detail := prDetailHandler(t, nil)
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		detail(w, r)
	})
	evals := policyEvaluationsHandler(t, evaluations)
	mux.HandleFunc("/org/project/_apis/policy/evaluations", func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		evals(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	got, err := provider.PullRequestCIFailures(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "42")
	if err != nil {
		t.Fatalf("PullRequestCIFailures: %v", err)
	}
	if got.HeadSHA != "head-sha" {
		t.Errorf("HeadSHA = %q, want the PR's evaluated source head", got.HeadSHA)
	}
	for _, req := range requests {
		if req[:4] != "GET " {
			t.Errorf("request %q: CI evidence collection must be read-only", req)
		}
	}
	if len(got.Failures) != 4 {
		t.Fatalf("failures = %+v, want the two rejected builds, the broken status and the unclassified policy only", got.Failures)
	}
	b, s, u, o := got.Failures[0], got.Failures[1], got.Failures[2], got.Failures[3]
	if b.Name != "Build: ci-validate" || b.Conclusion != "rejected" || b.State != CheckStateFailing {
		t.Errorf("build failure = %+v", b)
	}
	if want := server.URL + "/org/project/_build/results?buildId=314"; b.URL != want {
		t.Errorf("build URL = %q, want %q", b.URL, want)
	}
	if b.Summary == "" || b.Annotations == nil || len(b.Annotations) != 0 {
		t.Errorf("build failure summary/annotations = %q/%v, want a summary and empty annotations", b.Summary, b.Annotations)
	}
	if s.Name != "Status: example-ci/lint" || s.Conclusion != "broken" || s.URL != "" {
		t.Errorf("status failure = %+v, want the named broken status with no build link", s)
	}
	// An unnamed build policy is told apart by its configuration id.
	if u.Name != "Build #27" {
		t.Errorf("unnamed build failure name = %q, want %q", u.Name, "Build #27")
	}
	// Only build policies mention build logs.
	if !strings.Contains(b.Summary, "build logs") {
		t.Errorf("build summary = %q, want it to say build logs are not fetched", b.Summary)
	}
	for _, f := range []CIFailureDetail{s, o} {
		if strings.Contains(f.Summary, "build logs") {
			t.Errorf("%s summary = %q, want no mention of build logs for a non-build policy", f.Name, f.Summary)
		}
	}
	if o.Name != "Custom policy" || o.Summary != "blocking policy rejected by Azure DevOps" {
		t.Errorf("unclassified failure = %+v, want a neutral rejected summary", o)
	}
}
