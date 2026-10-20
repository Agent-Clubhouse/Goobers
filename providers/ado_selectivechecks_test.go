package providers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestADOListPullRequestsWithCheckFilterPreservesSnapshot(t *testing.T) {
	var policies atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"value": []map[string]any{
			{"pullRequestId": 1, "status": "active", "description": "Context", "repository": map[string]any{"project": map[string]string{"id": "project-id", "name": "project"}}},
			{"pullRequestId": 2, "status": "active", "isDraft": true, "repository": map[string]any{"project": map[string]string{"id": "project-id", "name": "project"}}},
		}})
	})
	mux.HandleFunc("/org/project/_apis/policy/evaluations", func(w http.ResponseWriter, r *http.Request) {
		policies.Add(1)
		if !strings.HasSuffix(r.URL.Query().Get("artifactId"), "/1") {
			t.Error("fetched policies for excluded metadata")
		}
		writeJSON(t, w, map[string]any{"value": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	req := ListPullRequestsRequest{Repository: RepositoryRef{Name: "repo", Project: "project"}}
	prs, err := p.ListPullRequestsWithCheckFilter(t.Context(), req, func(pr PullRequestSummary) (bool, error) { return !pr.Draft, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].CheckState != CheckStatePassing || prs[1].CheckState != "" || prs[0].State != "open" || prs[0].Body != "Context" {
		t.Fatalf("snapshot=%+v", prs)
	}
	if policies.Load() != 1 {
		t.Fatalf("policies=%d", policies.Load())
	}
	predicateErr := errors.New("predicate failed")
	_, err = p.ListPullRequestsWithCheckFilter(t.Context(), req, func(PullRequestSummary) (bool, error) { return false, predicateErr })
	if !errors.Is(err, predicateErr) || policies.Load() != 1 {
		t.Fatalf("predicate error=%v policies=%d", err, policies.Load())
	}
	req.SkipCheckState = true
	_, err = p.ListPullRequestsWithCheckFilter(t.Context(), req, func(PullRequestSummary) (bool, error) { t.Error("skip-checks called the filter"); return true, nil })
	if err != nil || policies.Load() != 1 {
		t.Fatalf("skip-checks error=%v policies=%d", err, policies.Load())
	}
}
